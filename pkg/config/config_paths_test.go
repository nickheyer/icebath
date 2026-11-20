package config

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPathsAndManagedModeFollowTheResolvedHome(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_MANAGED", "")
	ctx := context.Background()
	if GetConfigPath(h) != filepath.Join(h, "config.yaml") || GetEnvPath(h) != filepath.Join(h, ".env") {
		t.Fatalf("paths %s %s", GetConfigPath(h), GetEnvPath(h))
	}
	if IsManaged(ctx, h) {
		t.Fatal("unmanaged home reported managed")
	}
	if err := SaveEnvValueContext(ctx, h, "FIXTURE_KEY", "v"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_MANAGED", "nix")
	if !IsManaged(ctx, h) {
		t.Fatal("ICEBATH_MANAGED home reported unmanaged")
	}
	var refusal bytes.Buffer
	saved := EnvRefusalOut
	EnvRefusalOut = &refusal
	defer func() { EnvRefusalOut = saved }()
	_ = SaveEnvValueContext(ctx, h, "OTHER_KEY", "w")
	raw, err := os.ReadFile(GetEnvPath(h))
	if err != nil || !strings.Contains(string(raw), "FIXTURE_KEY=v") || strings.Contains(string(raw), "OTHER_KEY") {
		t.Fatalf(".env %q %v", raw, err)
	}
	if v, ok := (Tree{"a": map[string]any{"b.c": 1}}).Lookup("a.b.c"); !ok || v != 1 {
		t.Fatalf("lookup %v %v", v, ok)
	}
	if _, ok := (Tree{"a": map[string]any{}}).Lookup("a.missing"); ok {
		t.Fatal("missing key found")
	}
	if got := terminalEnvValueText([]any{"x", 1}); got != `["x", 1]` {
		t.Fatalf("terminal env text %q", got)
	}
}
