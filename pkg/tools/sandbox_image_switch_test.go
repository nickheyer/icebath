package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"gopkg.in/yaml.v3"
)

// Temp icebath home with HOME beside it and every TERMINAL_ variable restored after the test
func sandboxSwitchHome(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("HOME", tmp)
	saved := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "TERMINAL_") {
			saved[k] = v
		}
	}
	t.Cleanup(func() {
		for _, kv := range os.Environ() {
			if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "TERMINAL_") {
				os.Unsetenv(k)
			}
		}
		for k, v := range saved {
			os.Setenv(k, v)
		}
	})
	for _, v := range []string{"TERMINAL_ENV", "TERMINAL_DOCKER_IMAGE", "TERMINAL_DOCKER_IMAGE_PINNED"} {
		os.Unsetenv(v)
	}
	return h, tmp
}

// Writes a config.yaml holding only the terminal section
func writeSandboxSwitchConfig(t *testing.T, h string, terminal map[string]any) {
	t.Helper()
	raw, err := yaml.Marshal(map[string]any{"terminal": terminal})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Installs a docker script on PATH that prints PS_OUTPUT for ps and forgets the located docker CLI
func fakeSandboxDocker(t *testing.T, tmp, psOutput string) string {
	t.Helper()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "docker")
	script := "#!/bin/sh\ncase \"$1\" in ps) printf '%s' \"$PS_OUTPUT\";; *) exit 0;; esac\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PS_OUTPUT", psOutput)
	t.Setenv("ICEBATH_DOCKER_BINARY", "")
	os.Unsetenv("ICEBATH_DOCKER_BINARY")
	ResetDockerExecutable()
	t.Cleanup(ResetDockerExecutable)
	return exe
}

// Terminal section of config.yaml as the raw YAML document holds it
func rawSandboxSwitchTerminal(t *testing.T, h string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	terminal, _ := doc["terminal"].(map[string]any)
	return terminal
}

func TestBridgeMarksDefaultImageUnpinnedAndFileKeyPinned(t *testing.T) {
	h, _ := sandboxSwitchHome(t)
	ctx := context.Background()
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker"})
	env := map[string]string{}
	if err := config.ApplyTerminalConfigToEnv(ctx, h, config.TerminalEnvOptions{Env: env}); err != nil {
		t.Fatal(err)
	}
	if env["TERMINAL_DOCKER_IMAGE"] != config.DefaultSandboxImage || env["TERMINAL_DOCKER_IMAGE_PINNED"] != "0" {
		t.Fatalf("bridged %v", env)
	}
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker", "docker_image": config.DefaultSandboxImage})
	override := true
	env = map[string]string{}
	if err := config.ApplyTerminalConfigToEnv(ctx, h, config.TerminalEnvOptions{Env: env, Override: &override}); err != nil {
		t.Fatal(err)
	}
	if env["TERMINAL_DOCKER_IMAGE_PINNED"] != "1" {
		t.Fatalf("a file key equal to the default is still a pin %v", env)
	}
}

func TestBridgeTreatsAPresetEnvImageAsPinnedAndKeepsALauncherVerdict(t *testing.T) {
	h, _ := sandboxSwitchHome(t)
	ctx := context.Background()
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker"})
	env := map[string]string{"TERMINAL_DOCKER_IMAGE": "ghcr.io/me/mine:1"}
	if err := config.ApplyTerminalConfigToEnv(ctx, h, config.TerminalEnvOptions{Env: env}); err != nil {
		t.Fatal(err)
	}
	if env["TERMINAL_DOCKER_IMAGE_PINNED"] != "1" {
		t.Fatalf("an operator's env var is their choice %v", env)
	}
	env = map[string]string{"TERMINAL_DOCKER_IMAGE": "whatever", "TERMINAL_DOCKER_IMAGE_PINNED": "0"}
	if err := config.ApplyTerminalConfigToEnv(ctx, h, config.TerminalEnvOptions{Env: env}); err != nil {
		t.Fatal(err)
	}
	if env["TERMINAL_DOCKER_IMAGE_PINNED"] != "0" {
		t.Fatalf("the launcher verdict was replaced %v", env)
	}
}

func TestTerminalScopeRecomputesThePinPerProfile(t *testing.T) {
	h, tmp := sandboxSwitchHome(t)
	ctx := context.Background()
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker"})
	scope, err := BuildProfileTerminalScope(ctx, h, map[string]string{"TERMINAL_DOCKER_IMAGE_PINNED": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if scope["TERMINAL_DOCKER_IMAGE"] != config.DefaultSandboxImage || scope["TERMINAL_DOCKER_IMAGE_PINNED"] != "0" {
		t.Fatalf("the launch overlay's verdict is never inherited %v", scope)
	}
	other := filepath.Join(tmp, "profiles", "b")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSandboxSwitchConfig(t, other, map[string]any{"backend": "docker", "docker_image": "ghcr.io/me/mine:1"})
	if scope, err = BuildProfileTerminalScope(ctx, other, nil); err != nil || scope["TERMINAL_DOCKER_IMAGE_PINNED"] != "1" {
		t.Fatalf("file pinned profile %v %v", scope, err)
	}
	if err := os.WriteFile(filepath.Join(other, ".env"), []byte("TERMINAL_DOCKER_IMAGE=ghcr.io/me/env:2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSandboxSwitchConfig(t, other, map[string]any{"backend": "docker"})
	if scope, err = BuildProfileTerminalScope(ctx, other, nil); err != nil || scope["TERMINAL_DOCKER_IMAGE_PINNED"] != "1" {
		t.Fatalf("dotenv pinned profile %v %v", scope, err)
	}
}

func TestTerminalScopePinsAnImageWrittenInTheProfileEnvEvenWhenItSpellsTheDefault(t *testing.T) {
	h, _ := sandboxSwitchHome(t)
	ctx := context.Background()
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker"})
	envPath := filepath.Join(h, ".env")
	if err := os.WriteFile(envPath, []byte("TERMINAL_DOCKER_IMAGE="+config.DefaultSandboxImage+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope, err := BuildProfileTerminalScope(ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope["TERMINAL_DOCKER_IMAGE"] != config.DefaultSandboxImage || scope["TERMINAL_DOCKER_IMAGE_PINNED"] != "1" {
		t.Fatalf("default spelled in .env %v", scope)
	}
	if err := os.WriteFile(envPath, []byte("TERMINAL_DOCKER_IMAGE=ghcr.io/me/mine:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if scope, err = BuildProfileTerminalScope(ctx, h, nil); err != nil || scope["TERMINAL_DOCKER_IMAGE_PINNED"] != "1" {
		t.Fatalf("own image in .env %v %v", scope, err)
	}
	if err := os.WriteFile(envPath, []byte("OTHER=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if scope, err = BuildProfileTerminalScope(ctx, h, nil); err != nil || scope["TERMINAL_DOCKER_IMAGE_PINNED"] != "0" {
		t.Fatalf("no image in .env %v %v", scope, err)
	}
}

func TestPendingNeedsDockerBackendUnpinnedAndAStaleContainer(t *testing.T) {
	h, tmp := sandboxSwitchHome(t)
	ctx := context.Background()
	env := &Env{Home: h}
	fakeSandboxDocker(t, tmp, "icebath-abc\t"+config.LegacySandboxImage+"\nicebath-def\t"+config.DefaultSandboxImage+"\n")
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "local"})
	if p, err := PendingSandboxImageSwitch(ctx, env); err != nil || p != nil {
		t.Fatalf("local backend has no sandbox to switch %+v %v", p, err)
	}
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker", "docker_image": config.LegacySandboxImage})
	if p, err := PendingSandboxImageSwitch(ctx, env); err != nil || p != nil {
		t.Fatalf("a pinned image is a decision already made %+v %v", p, err)
	}
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker"})
	p, err := PendingSandboxImageSwitch(ctx, env)
	if err != nil || p == nil {
		t.Fatalf("pending %+v %v", p, err)
	}
	if p.CurrentImage != config.LegacySandboxImage || p.TargetImage != config.DefaultSandboxImage || !slices.Equal(p.Containers, []string{"icebath-abc"}) {
		t.Fatalf("pending %+v", p)
	}
	fakeSandboxDocker(t, tmp, "icebath-def\t"+config.DefaultSandboxImage+"\n")
	if p, err := PendingSandboxImageSwitch(ctx, env); err != nil || p != nil {
		t.Fatalf("every container already on the target %+v %v", p, err)
	}
}

func TestDecidePinsAndEitherAnswerEndsTheOffer(t *testing.T) {
	h, tmp := sandboxSwitchHome(t)
	ctx := context.Background()
	env := &Env{Home: h}
	fakeSandboxDocker(t, tmp, "icebath-abc\t"+config.LegacySandboxImage+"\n")
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker"})
	evicted := []string{}
	saved := sandboxSwitchEvict
	sandboxSwitchEvict = func(_ *Env, task string) error { evicted = append(evicted, task); return nil }
	t.Cleanup(func() { sandboxSwitchEvict = saved })

	p, err := PendingSandboxImageSwitch(ctx, env)
	if err != nil || p == nil {
		t.Fatalf("pending %+v %v", p, err)
	}
	if image, err := DecideSandboxImageSwitch(ctx, env, p, false); err != nil || image != config.LegacySandboxImage {
		t.Fatalf("keep %q %v", image, err)
	}
	if got := rawSandboxSwitchTerminal(t, h)["docker_image"]; got != config.LegacySandboxImage {
		t.Fatalf("keep = pin the running image, got %v", got)
	}
	if p, err := PendingSandboxImageSwitch(ctx, env); err != nil || p != nil || len(evicted) != 0 {
		t.Fatalf("after keep %+v %v evicted %v", p, err, evicted)
	}

	for _, v := range []string{"TERMINAL_DOCKER_IMAGE", "TERMINAL_DOCKER_IMAGE_PINNED"} {
		os.Unsetenv(v)
	}
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker"})
	if p, err = PendingSandboxImageSwitch(ctx, env); err != nil || p == nil {
		t.Fatalf("pending after unpin %+v %v", p, err)
	}
	if image, err := DecideSandboxImageSwitch(ctx, env, p, true); err != nil || image != config.DefaultSandboxImage {
		t.Fatalf("approve %q %v", image, err)
	}
	if got := rawSandboxSwitchTerminal(t, h)["docker_image"]; got != config.DefaultSandboxImage {
		t.Fatalf("approve = pin the target even though it equals the default, got %v", got)
	}
	if got := os.Getenv("TERMINAL_DOCKER_IMAGE_PINNED"); got != "1" {
		t.Fatalf("the live process is re-bridged, pinned %q", got)
	}
	if !slices.Equal(evicted, []string{""}) {
		t.Fatalf("the cached env is dropped so the next terminal call recreates, evicted %q", evicted)
	}
	if p, err := PendingSandboxImageSwitch(ctx, env); err != nil || p != nil {
		t.Fatalf("after approve %+v %v", p, err)
	}
}

func TestPendingReadsABoundTerminalScopeNotTheLaunchEnv(t *testing.T) {
	h, tmp := sandboxSwitchHome(t)
	ctx := context.Background()
	env := &Env{Home: h}
	fakeSandboxDocker(t, tmp, "icebath-abc\t"+config.LegacySandboxImage+"\n")
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker"})
	os.Setenv("TERMINAL_DOCKER_IMAGE", "ghcr.io/launch-profile/pinned:1")
	if p, err := PendingSandboxImageSwitch(ctx, env); err != nil || p != nil {
		t.Fatalf("launch env pin %+v %v", p, err)
	}
	scoped := SetTerminalScope(ctx, &TerminalScope{Policy: TerminalPolicy{"TERMINAL_ENV": "docker", "TERMINAL_DOCKER_IMAGE_PINNED": "0"}})
	if p, err := PendingSandboxImageSwitch(scoped, env); err != nil || p == nil {
		t.Fatalf("the routed profile's scope says unpinned, the launch env is not consulted %+v %v", p, err)
	}
}

func TestPendingSandboxImageSwitchOddInputs(t *testing.T) {
	h, tmp := sandboxSwitchHome(t)
	ctx := context.Background()
	env := &Env{Home: h}
	fakeSandboxDocker(t, tmp, "a\tx:1\nb\tx:1\nc\ty:2\nnotab\n\tz:3\n")
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker"})
	p, err := PendingSandboxImageSwitch(ctx, env)
	if err != nil || p == nil || p.CurrentImage != "x:1" || !slices.Equal(p.Containers, []string{"", "a", "b", "c"}) {
		t.Fatalf("most containers wins %+v %v", p, err)
	}
	fakeSandboxDocker(t, tmp, "c\ty:2\na\tx:1\n")
	if p, err = PendingSandboxImageSwitch(ctx, env); err != nil || p == nil || p.CurrentImage != "y:2" {
		t.Fatalf("a tie keeps the first listed image %+v %v", p, err)
	}
	writeSandboxSwitchConfig(t, h, map[string]any{"backend": "docker", "docker_shared_container_key": "team"})
	if p, err = PendingSandboxImageSwitch(ctx, env); err != nil || p == nil {
		t.Fatalf("shared key %+v %v", p, err)
	}
	ResetDockerExecutable()
	t.Setenv("PATH", filepath.Join(tmp, "nothing"))
	if p, err = PendingSandboxImageSwitch(ctx, env); err != nil || p != nil {
		t.Fatalf("no docker on PATH %+v %v", p, err)
	}
}

func TestSandboxImageSwitchExplainLines(t *testing.T) {
	one := SandboxImageSwitchExplainLines(&SandboxImageSwitch{CurrentImage: "old:1", TargetImage: "new:2", Containers: []string{"a"}})
	want := []string{
		"Your Docker sandbox (1 container) runs old:1.",
		"The default sandbox image is now new:2: Python 3.13, Node 26, and a desktop",
		"so Bot Screen, computer_use and the browser run inside the sandbox instead of on this machine.",
		"Switching recreates the container on the next terminal call. Files in /root and /workspace",
		"stay (they live on this machine); packages installed with apt/pip/npm -g inside the container",
		"are reinstalled on demand; Python 3.11 virtualenvs need a rebuild.",
	}
	if !slices.Equal(one, want) {
		t.Fatalf("lines %q", one)
	}
	two := SandboxImageSwitchExplainLines(&SandboxImageSwitch{CurrentImage: "old:1", TargetImage: "new:2", Containers: []string{"a", "b"}})
	if two[0] != "Your Docker sandbox (2 containers) runs old:1." {
		t.Fatalf("plural %q", two[0])
	}
	none := SandboxImageSwitchExplainLines(&SandboxImageSwitch{CurrentImage: "old:1", TargetImage: "new:2"})
	if none[0] != "Your Docker sandbox (0 containers) runs old:1." {
		t.Fatalf("zero %q", none[0])
	}
}
