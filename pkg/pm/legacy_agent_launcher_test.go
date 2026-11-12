package pm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyAgentConvenienceRunsTheLegacyRunner(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	published, err := publishConveniences(root, out, []string{"icebath", "icebath-agent"}, true)
	if err != nil || len(published) != 2 {
		t.Fatalf("published %v err %v", published, err)
	}
	launcher := filepath.Join(root, ".icebath", "bin", "icebath")
	for name, want := range map[string]string{
		"icebath":       "#!/bin/sh\nexec " + launcher + " \"$@\"\n",
		"icebath-agent": "#!/bin/sh\nexec " + launcher + " " + LegacyAgentEntry + " \"$@\"\n",
	} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s: %q err %v want %q", name, data, err, want)
		}
		if !ownsLauncher(filepath.Join(out, name), root) {
			t.Fatalf("%s is not recognised as this install's launcher", name)
		}
	}
}
