package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWritebackRoundtripByteIdenticalWhenUnchanged(t *testing.T) {
	h := filepath.Join(t.TempDir(), "icebath_home")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	ResetCache()
	t.Cleanup(ResetCache)
	original := "custom_prompt: keep ${NOT_SET_VAR}\ndisplay:\n  skin: usertheme\n"
	p := filepath.Join(h, "config.yaml")
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	data, err := ReadUserConfigRaw(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatalf("read rewrote the file %q", after)
	}
	if data["custom_prompt"] != "keep ${NOT_SET_VAR}" {
		t.Fatalf("parsed %#v", data["custom_prompt"])
	}
}
