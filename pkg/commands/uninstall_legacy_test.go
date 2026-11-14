package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestLegacyCleanupRemovesOnlyRuntimeBytesAndIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	runtimeFiles := []string{"node/bin/node", "bin/uv", "bin/uv.exe"}
	userFiles := []string{"bin/my-script", "config.yaml", "auth.json", "SOUL.md", "sessions/session", "skills/demo/SKILL.md",
		"memories/MEMORY.md", "profiles/other/config.yaml"}
	for _, name := range append(append([]string{}, runtimeFiles...), userFiles...) {
		writeTestFile(t, filepath.Join(tmp, name), []byte(name))
	}
	var out bytes.Buffer
	removed := RemoveLegacyRuntimeTrees(tmp, &out)
	sort.Strings(removed)
	want := []string{filepath.Join(tmp, "bin", "uv"), filepath.Join(tmp, "bin", "uv.exe"), filepath.Join(tmp, "node")}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed %v", removed)
	}
	for _, name := range runtimeFiles {
		if _, err := os.Stat(filepath.Join(tmp, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survives", name)
		}
	}
	for _, name := range userFiles {
		if data, err := os.ReadFile(filepath.Join(tmp, name)); err != nil || string(data) != name {
			t.Fatalf("%s changed %q %v", name, data, err)
		}
	}
	if again := RemoveLegacyRuntimeTrees(tmp, &out); len(again) != 0 {
		t.Fatalf("second run %v", again)
	}
	if out.Len() != 0 {
		t.Fatalf("warnings %q", out.String())
	}
}
