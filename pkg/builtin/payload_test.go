package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

var refsRoot = filepath.Join("..", "..", "references", "projects")

func TestEmbeddedPayloadEqualsManifest(t *testing.T) {
	entries, err := manifestEntries()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, e := range entries {
		want[e.Path] = e.Sum
	}
	got := map[string]string{}
	err = fs.WalkDir(payloadFS, "payload", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := payloadFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		got[strings.TrimPrefix(p, "payload/")] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 401 || len(want) != 401 {
		t.Fatalf("embedded %d files, manifest %d, want 401", len(got), len(want))
	}
	for p, sum := range want {
		if got[p] != sum {
			t.Errorf("%s: embedded %q manifest %q", p, got[p], sum)
		}
	}
}

func TestManifestEqualsReferenceTree(t *testing.T) {
	sources := map[string][]string{"elephant-goldfish-model": {"skills", "agents", "resources", "docs", "plugin.json", "README.md", "LICENSE", "TESTING.md", "AGENTS.md", "scripts/pack_plugin.py", "assets", ".codex-plugin/plugin.json"},
		"hallmark": {"skills", ".claude-plugin"}, "impeccable": {"plugin", "skill"}}
	var lines []string
	for project, entries := range sources {
		for _, entry := range entries {
			err := filepath.WalkDir(filepath.Join(refsRoot, project, entry), func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return err
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				info, err := os.Stat(p)
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(refsRoot, p)
				mode := "0644"
				if info.Mode()&0o100 != 0 || strings.HasPrefix(string(data), "#!") {
					mode = "0755"
				}
				sum := sha256.Sum256(data)
				lines = append(lines, fmt.Sprintf("%s %s %s", filepath.ToSlash(rel), hex.EncodeToString(sum[:]), mode))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	sort.Strings(lines)
	if want := strings.Join(lines, "\n") + "\n"; want != payloadManifest {
		t.Fatalf("payload_manifest.txt drifted from the reference tree, rerun go generate")
	}
}

func TestMaterializeWritesTreeOnceWithModes(t *testing.T) {
	cache := t.TempDir()
	tree, err := materializeAt(cache)
	if err != nil {
		t.Fatal(err)
	}
	again, err := materializeAt(cache)
	if err != nil || again.Root != tree.Root {
		t.Fatalf("second materialize %v %v", again, err)
	}
	if !strings.HasPrefix(filepath.Base(tree.Root), plugins.RunningVersion+"-") || filepath.Dir(tree.Root) != filepath.Join(cache, "builtin-plugins") {
		t.Fatalf("root %s", tree.Root)
	}
	for rel, mode := range map[string]fs.FileMode{"impeccable/skill/scripts/impeccable": 0o755, "hallmark/skills/hallmark/scripts/sloplint.mjs": 0o755,
		"hallmark/skills/hallmark/SKILL.md": 0o644, "elephant-goldfish-model/plugin.json": 0o644} {
		info, err := os.Stat(tree.Payload(rel))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode %v want %v", rel, info.Mode().Perm(), mode)
		}
		data, _ := os.ReadFile(tree.Payload(rel))
		embedded, _ := payloadFile(rel)
		if string(data) != string(embedded) {
			t.Errorf("%s bytes differ", rel)
		}
	}
	for _, dir := range []string{tree.Root, tree.Payload("impeccable/skill"), tree.Host("egm/skills/egm-review")} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s dir mode %v %v", dir, info, err)
		}
	}
	if info, err := os.Stat(tree.Host("impeccable/skills/impeccable/scripts/impeccable")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("rendered launcher %v %v", info, err)
	}
}

func TestConcurrentFirstMaterializeKeepsOneTree(t *testing.T) {
	cache := t.TempDir()
	dir := filepath.Join(cache, "builtin-plugins", "x-"+payloadDigest())
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, errs[i] = writeTree(dir)
			} else {
				_, errs[i] = materializeAt(filepath.Join(cache, "second"))
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(cache, "builtin-plugins", "*.tmp-*"))
	if len(leftovers) != 0 || !isFile(filepath.Join(dir, ".complete")) {
		t.Fatalf("leftovers %v", leftovers)
	}
}
