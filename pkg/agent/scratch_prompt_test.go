package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestBootHookAndPromptHandThePruneToOneChild(t *testing.T) {
	spawned, restore := home.CaptureScratchPruneSpawns()
	t.Cleanup(restore)
	tmp := t.TempDir()
	launch := filepath.Join(tmp, "launch")
	t.Setenv(home.EnvHome, launch)
	scratch := filepath.Join(launch, "cache", "scratch")
	env := map[string]string{home.EnvHome: launch}
	if !home.ApplyScratchTmpEnv(env) {
		t.Fatal("boot hook did not point the temp vars at the scratch dir")
	}
	hints, err := localHostHints(&tools.Env{Home: home.Dir(context.Background()), Cwd: tmp})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(scratch); env["TMPDIR"] != scratch || err != nil || !st.IsDir() {
		t.Fatalf("TMPDIR %q, scratch dir %v", env["TMPDIR"], err)
	}
	if !strings.Contains(hints[0], "Scratch directory: "+scratch+" (TMPDIR points here") {
		t.Fatalf("hints %q", hints[0])
	}
	if !reflect.DeepEqual(*spawned, []string{scratch}) {
		t.Fatalf("spawned %v, want one prune of %s", *spawned, scratch)
	}
	home.ResetScratchPruneLatch()
	routed := filepath.Join(tmp, "routed")
	ctx := home.WithOverride(context.Background(), routed)
	if got := home.EnsureScratchDir(home.Dir(ctx)); got != filepath.Join(routed, "cache", "scratch") {
		t.Fatalf("routed scratch %q", got)
	}
	if !reflect.DeepEqual(*spawned, []string{scratch, filepath.Join(routed, "cache", "scratch")}) {
		t.Fatalf("spawned %v", *spawned)
	}
}
