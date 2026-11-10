package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func (c *configHome) unset(key string) error {
	return config.UnsetConfigValue(context.Background(), c.h, key, configCommandSources(c.h))
}

// Corrupt snapshots the refused write left under the home
func (c *configHome) corruptBackups() []string {
	c.t.Helper()
	found, err := filepath.Glob(filepath.Join(c.h, "backups", "config", "config.yaml.corrupt.*"))
	if err != nil {
		c.t.Fatal(err)
	}
	return found
}

// Asserts err is the RuntimeError a refused config write raises and that its text carries match
func (c *configHome) refused(err error, match string) {
	c.t.Helper()
	var runtimeErr *config.RuntimeError
	if !errors.As(err, &runtimeErr) || !strings.Contains(err.Error(), match) {
		c.t.Fatalf("refusal %v, want a RuntimeError matching %q", err, match)
	}
}

func TestSaveAndLoadRoundtripConfigSetRefusesToOverwriteUnparseableExistingConfig(t *testing.T) {
	c := newConfigHome(t)
	original := "model:\n  default: claude-opus\n  provider: anthropic\ngateway:\n  platforms:\n    telegram:\n      enabled: true\nbroken: [unterminated\n"
	c.write(original)
	c.refused(c.set("model.default", "gpt-4o", false), "formatting error")
	if got := c.readConfig(); got != original {
		t.Fatalf("config changed: %q", got)
	}
	if len(c.corruptBackups()) == 0 {
		t.Fatal("parse-failure path should snapshot a corrupt backup before refusing")
	}
}

func TestSaveAndLoadRoundtripConfigUnsetRefusesToOverwriteUnparseableExistingConfig(t *testing.T) {
	c := newConfigHome(t)
	original := "model:\n  default: keep-me\nbroken: [unterminated\n"
	c.write(original)
	if err := os.WriteFile(filepath.Join(c.h, ".env"), []byte("TERMINAL_TIMEOUT=30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.refused(c.unset("terminal.timeout"), "formatting error")
	if got := c.readConfig(); got != original {
		t.Fatalf("config changed: %q", got)
	}
	if got := c.readEnv(); got != "TERMINAL_TIMEOUT=30\n" {
		t.Fatalf(".env changed: %q", got)
	}
	if len(c.corruptBackups()) == 0 {
		t.Fatal("unset parse-failure path should snapshot a corrupt backup before refusing")
	}
}

func TestSaveAndLoadRoundtripConfigSetRefusesNonMappingRoot(t *testing.T) {
	c := newConfigHome(t)
	original := "- just\n- a\n- list\n"
	c.write(original)
	c.refused(c.set("model.default", "gpt-4o", false), "must be a mapping")
	if got := c.readConfig(); got != original {
		t.Fatalf("config changed: %q", got)
	}
	if len(c.corruptBackups()) == 0 {
		t.Fatal("non-mapping root should snapshot a corrupt backup before refusing")
	}
}

func TestSaveAndLoadRoundtripConfigUnsetRefusesNonMappingRoot(t *testing.T) {
	c := newConfigHome(t)
	original := "- just\n- a\n- list\n"
	c.write(original)
	c.refused(c.unset("model.default"), "must be a mapping")
	if got := c.readConfig(); got != original {
		t.Fatalf("config changed: %q", got)
	}
	if len(c.corruptBackups()) == 0 {
		t.Fatal("no corrupt backup")
	}
}

func TestSaveAndLoadRoundtripConfigSetAllowsValidEmptyMapping(t *testing.T) {
	c := newConfigHome(t)
	c.write("{}\n")
	c.mustSet("model.default", "gpt-4o", false)
	if saved := c.parsed(); !reflect.DeepEqual(saved, map[string]any{"model": map[string]any{"default": "gpt-4o"}}) {
		t.Fatalf("saved %v", saved)
	}
}

func TestConfigCommandFailClosedSurfaceConfigCommandSetExitsCleanlyOnBrokenYaml(t *testing.T) {
	c := newConfigHome(t)
	original := "model:\n  default: keep\nbroken: [unterminated\n"
	c.write(original)
	err := c.command(config.ConfigArgs{Command: "set", Key: "model.default", Value: strPtr("gpt-4o"), Force: false})
	if exitStatus(err) != 1 {
		t.Fatalf("exit %v", err)
	}
	if _, errOut := c.drain(); !strings.Contains(errOut, "formatting error") || !strings.Contains(errOut, "`icebath config edit`") {
		t.Fatalf("stderr %q", errOut)
	}
	if got := c.readConfig(); got != original {
		t.Fatalf("config changed: %q", got)
	}
}

func TestConfigCommandFailClosedSurfaceConfigCommandUnsetExitsCleanlyOnBrokenYaml(t *testing.T) {
	c := newConfigHome(t)
	original := "model:\n  default: keep\nbroken: [unterminated\n"
	c.write(original)
	err := c.command(config.ConfigArgs{Command: "unset", Key: "model.default"})
	if exitStatus(err) != 1 {
		t.Fatalf("exit %v", err)
	}
	if _, errOut := c.drain(); !strings.Contains(errOut, "formatting error") {
		t.Fatalf("stderr %q", errOut)
	}
	if got := c.readConfig(); got != original {
		t.Fatalf("config changed: %q", got)
	}
}
