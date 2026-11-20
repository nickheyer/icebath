package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestPersonalityCompletionsMemoConfigEditOnDiskRefreshesCompletions(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	personalitiesMemo = nil
	t.Cleanup(func() { personalitiesMemo = nil })
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	write := func(body string, stamp int64) {
		if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		at := time.Unix(stamp, 0)
		if err := os.Chtimes(cfgPath, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write("agent:\n  personalities:\n    zzfirst: v1\n", 1_700_000_000)
	oldLoad, oldPath := LoadCLIConfig, getConfigPath
	t.Cleanup(func() { LoadCLIConfig, getConfigPath = oldLoad, oldPath })
	LoadCLIConfig = func(context.Context) map[string]any {
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	getConfigPath = func(context.Context) string { return cfgPath }
	names := func() map[string]bool {
		out := map[string]bool{}
		personalityCompletions(context.Background(), "zz", func(c Completion) { out[c.Text] = true })
		return out
	}
	if !names()["zzfirst"] {
		t.Fatal("zzfirst missing before the edit")
	}
	write("agent:\n  personalities:\n    zzsecond: v2\n", 1_800_000_000)
	after := names()
	if !after["zzsecond"] || after["zzfirst"] {
		t.Fatalf("after %v", after)
	}
}
