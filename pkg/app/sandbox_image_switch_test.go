package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
