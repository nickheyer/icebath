package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersonalityCompletionsMemoConfigEditOnDiskRefreshesCompletions(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	cfgPath := filepath.Join(h, "config.yaml")
	write := func(body string, stamp int64) {
		if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		at := time.Unix(stamp, 0)
		if err := os.Chtimes(cfgPath, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write("personalities:\n  zzfirst: v1\n", 1_700_000_000)
	names := func() map[string]bool {
		out := map[string]bool{}
		personalityCompletions(context.Background(), "zz", func(c Completion) { out[c.Text] = true })
		return out
	}
	if !names()["zzfirst"] {
		t.Fatal("zzfirst missing before the edit")
	}
	write("personalities:\n  zzsecond: v2\n", 1_800_000_000)
	after := names()
	if !after["zzsecond"] || after["zzfirst"] {
		t.Fatalf("after %v", after)
	}
}
