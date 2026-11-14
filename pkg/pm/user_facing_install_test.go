package pm

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Node whose staged entry always verifies, the user facing test's stand in for a real runtime
type catTrustedNode struct{ *Nodejs }

func (p *catTrustedNode) Verify(entry, target string) string { return "" }

// Python whose staged entry always verifies, the user facing test's stand in for a real interpreter
type catTrustedPython struct{ *Python }

func (p *catTrustedPython) Verify(entry, target string) string { return "" }

func TestCatASealedWindowsPayloadSwapsPackagedToolsForWritableCopies(t *testing.T) {
	tmp := t.TempDir()
	shipped, writable := filepath.Join(tmp, "payload", "tools"), filepath.Join(tmp, "home", "tools")
	writeFile(t, filepath.Join(tmp, "payload", "manifest.json"), []byte("{}"), 0o666)
	t.Setenv(home.EnvHome, filepath.Join(tmp, "home"))
	t.Setenv(EnvRuntimeDir, shipped)
	config.ResetCache()
	target := catTarget(t)
	digest := "1111111111111111111111111111111111111111111111111111111111111111"
	lock := NewLockfile(filepath.Join(tmp, "lock.json"))
	facts, err := NewFacts(filepath.Join(shipped, "facts.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	bins := map[string]map[string]string{}
	trusted := []Package{&catTrustedNode{newNodejs()}, &catTrustedPython{newPython()}}
	for _, p := range trusted {
		name := p.Base().Name
		original := catPackage(t, name)
		t.Cleanup(func() { MustRegister(original) })
		MustRegister(p)
		lock.SetPin(name, "test", map[string]any{target: map[string]any{"url": "https://test.invalid", "sha256": digest}})
		entry := filepath.Join(shipped, p.StoreEntry("test", target))
		writeFile(t, p.Binary(entry, target), []byte("pinned "+name), 0o755)
		tree, err := TreeDigest(entry)
		if err != nil {
			t.Fatal(err)
		}
		recordTarget := target
		if err := facts.Record(name, "test", filepath.Base(entry), p.Env(entry, target), shipped,
			FactRecord{Target: &recordTarget, Artifacts: []string{digest}, Digest: &tree}); err != nil {
			t.Fatal(err)
		}
		bins[name] = map[string]string{}
		for _, root := range []string{shipped, writable} {
			bins[name][root] = filepath.Dir(p.Binary(filepath.Join(root, filepath.Base(entry)), target))
		}
	}
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	catSwap(t, &lockfilePath, func() string { return lock.Path })
	catSwap(t, &userFacingApplies, func(context.Context) (bool, error) { return true, nil })
	path := []string{bins["python"][shipped], bins["node"][shipped], "/usr/bin"}

	t.Setenv(EnvDisableLazyInstalls, "1")
	if got := catUserFacing(t, path...); !slices.Equal(got, []string{"/usr/bin", bins["python"][shipped], bins["node"][shipped]}) {
		t.Fatalf("lazy installs off = %q", got)
	}
	if sttExists(writable) {
		t.Fatal("a copy was made with lazy installs off")
	}

	os.Unsetenv(EnvDisableLazyInstalls)
	if got := catUserFacing(t, path...); !slices.Equal(got, []string{"/usr/bin", bins["python"][writable], bins["node"][writable]}) {
		t.Fatalf("lazy installs on = %q", got)
	}
	copied, err := Copies(t.Context(), []string{bins["node"][shipped]})
	if err != nil || !slices.Equal(copied, []string{bins["node"][writable]}) {
		t.Fatalf("copies = %q, %v", copied, err)
	}
}
