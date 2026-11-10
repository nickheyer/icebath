package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
	"gopkg.in/yaml.v3"
)

// Temp home with a fake docker listing one legacy container, TERMINAL_ variables restored after the test
func sandboxOfferHome(t *testing.T) (string, *tools.Env) {
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
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1\" in ps) printf '%s' \"$PS_OUTPUT\";; *) exit 0;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PS_OUTPUT", "icebath-abc\t"+config.LegacySandboxImage+"\n")
	t.Setenv("ICEBATH_DOCKER_BINARY", "")
	os.Unsetenv("ICEBATH_DOCKER_BINARY")
	tools.ResetDockerExecutable()
	t.Cleanup(tools.ResetDockerExecutable)
	raw, err := yaml.Marshal(map[string]any{"terminal": map[string]any{"backend": "docker"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return h, &tools.Env{Home: h}
}

// Terminal section of config.yaml as the raw YAML document holds it
func sandboxOfferTerminal(t *testing.T, h string) map[string]any {
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

// Answer function returning a fixed reply
func sandboxOfferAnswer(reply string) func(string) (string, error) {
	return func(string) (string, error) { return reply, nil }
}

func TestInteractiveOfferWritesOnlyOnAYesOrNo(t *testing.T) {
	h, env := sandboxOfferHome(t)
	ctx := context.Background()
	var printed []string
	cprint := func(s string) { printed = append(printed, s) }

	got, err := offerSandboxImageSwitchWith(ctx, env, cprint, sandboxOfferAnswer(""), io.Discard)
	if err != nil || got != nil {
		t.Fatalf("Enter = ask later %v %v", got, err)
	}
	if _, pinned := sandboxOfferTerminal(t, h)["docker_image"]; pinned {
		t.Fatal("Enter = ask later wrote docker_image")
	}
	stated := false
	for _, line := range printed {
		stated = stated || strings.Contains(line, "/root and /workspace")
	}
	if !stated {
		t.Fatalf("the offer states what carries over %q", printed)
	}

	got, err = offerSandboxImageSwitchWith(ctx, env, cprint, sandboxOfferAnswer("y"), io.Discard)
	if err != nil || got == nil || !*got {
		t.Fatalf("yes %v %v", got, err)
	}
	if image := sandboxOfferTerminal(t, h)["docker_image"]; image != config.DefaultSandboxImage {
		t.Fatalf("yes pinned %v", image)
	}

	got, err = offerSandboxImageSwitchWith(ctx, env, cprint, sandboxOfferAnswer("y"), io.Discard)
	if err != nil || got != nil {
		t.Fatalf("pinned: never asked again %v %v", got, err)
	}
}

func TestOfferSandboxImageSwitchKeepInterruptAndTranscript(t *testing.T) {
	h, env := sandboxOfferHome(t)
	ctx := context.Background()
	var printed []string
	var asked []string
	cprint := func(s string) { printed = append(printed, s) }
	var stdout bytes.Buffer
	for _, stop := range []error{cliui.ErrInterrupt, cliui.ErrEOF} {
		stdout.Reset()
		got, err := offerSandboxImageSwitchWith(ctx, env, cprint, func(p string) (string, error) { asked = append(asked, p); return "", stop }, &stdout)
		if err != nil || got != nil || stdout.String() != "\n" {
			t.Fatalf("%v answer %v %v %q", stop, got, err, stdout.String())
		}
	}
	boom := errors.New("tty gone")
	if _, err := offerSandboxImageSwitchWith(ctx, env, cprint, func(string) (string, error) { return "", boom }, io.Discard); !errors.Is(err, boom) {
		t.Fatalf("other ask errors propagate %v", err)
	}
	printed = nil
	got, err := offerSandboxImageSwitchWith(ctx, env, cprint, sandboxOfferAnswer("  No "), io.Discard)
	if err != nil || got == nil || *got {
		t.Fatalf("no %v %v", got, err)
	}
	if image := sandboxOfferTerminal(t, h)["docker_image"]; image != config.LegacySandboxImage {
		t.Fatalf("no pinned %v", image)
	}
	want := []string{
		"",
		"☤ A new default sandbox image is available.",
		"  Your Docker sandbox (1 container) runs " + config.LegacySandboxImage + ".",
		"  The default sandbox image is now " + config.DefaultSandboxImage + ": Python 3.13, Node 26, and a desktop",
		"  so Bot Screen, computer_use and the browser run inside the sandbox instead of on this machine.",
		"  Switching recreates the container on the next terminal call. Files in /root and /workspace",
		"  stay (they live on this machine); packages installed with apt/pip/npm -g inside the container",
		"  are reinstalled on demand; Python 3.11 virtualenvs need a rebuild.",
		"  ✓ terminal.docker_image = " + config.LegacySandboxImage + "; unset it in config.yaml to be asked again.",
	}
	if strings.Join(printed, "\n") != strings.Join(want, "\n") {
		t.Fatalf("transcript\n%s", strings.Join(printed, "\n"))
	}
	if len(asked) != 2 || asked[0] != "  Switch now? [y = switch / n = keep the current image / Enter = ask later]: " {
		t.Fatalf("asked %q", asked)
	}
	_, env = sandboxOfferHome(t)
	printed = nil
	if got, err := offerSandboxImageSwitchWith(ctx, env, cprint, sandboxOfferAnswer("maybe"), io.Discard); err != nil || got != nil {
		t.Fatalf("later %v %v", got, err)
	}
	if last := printed[len(printed)-1]; last != "  Later. `icebath config set terminal.docker_image <image>` decides it any time." {
		t.Fatalf("later line %q", last)
	}
}
