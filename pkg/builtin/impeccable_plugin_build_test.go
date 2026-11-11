package builtin

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

var impeccableRepoRoot = filepath.Join(refsRoot, "impeccable")

// Temp build root holding the skill source, both root manifests, package.json and ENGINE_VERSION of the reference checkout
func impeccableBuildRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := copyDirSync(filepath.Join(impeccableRepoRoot, "skill"), filepath.Join(root, "skill")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".claude-plugin/plugin.json", ".claude-plugin/marketplace.json", "ENGINE_VERSION", "package.json"} {
		data, err := os.ReadFile(filepath.Join(impeccableRepoRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(rel)), string(data))
	}
	return root
}

func writeTestFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Slash relative paths of every file under a directory mapped to their permission bits
func treeModes(t *testing.T, dir string) map[string]fs.FileMode {
	t.Helper()
	out := map[string]fs.FileMode{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = info.Mode().Perm()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestImpeccablePluginBuildReproducesCommittedSubtree(t *testing.T) {
	root := impeccableBuildRoot(t)
	var out, errOut bytes.Buffer
	if err := buildImpeccableClaudePlugin(root, &out, &errOut); err != nil {
		t.Fatalf("build: %v\n%s%s", err, out.String(), errOut.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("build reported problems\n%s", errOut.String())
	}
	for _, line := range []string{
		"\U0001F528 Building cross-provider design skills...\n",
		"\U0001F4D6 Read 1 skills (1 user-invocable) and 12 pattern categories\n",
		"✓ Engine pinned at v" + strings.TrimSpace(readTestFile(t, filepath.Join(impeccableRepoRoot, "ENGINE_VERSION"))) + " (skill/scripts/VERSION)",
		"✓ Claude Code: 1 skill (45 reference files) (12 script files) (4 agent files) (settings.json)",
		"📦 Built Claude Code / Grok Build plugin subtree at ./plugin/",
		"✓ Plugin/skill versions agree: " + must(impeccableVersion()),
		"✓ Plugin manifest shape matches the verified loader contract",
		"✓ Batch launchers ship with CRLF line endings",
	} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Errorf("build log lacks %q\n%s", line, out.String())
		}
	}

	built := filepath.Join(root, "plugin")
	committed := filepath.Join(impeccableRepoRoot, "plugin")
	got, want := treeModes(t, built), treeModes(t, committed)
	if !slices.Equal(sortedKeys(got), sortedKeys(want)) {
		t.Fatalf("built plugin files\n%v\ncommitted\n%v", sortedKeys(got), sortedKeys(want))
	}
	for _, rel := range sortedKeys(want) {
		if readTestFile(t, filepath.Join(built, filepath.FromSlash(rel))) != readTestFile(t, filepath.Join(committed, filepath.FromSlash(rel))) {
			t.Errorf("%s differs from the committed plugin subtree", rel)
		}
		if got[rel]&0o111 != want[rel]&0o111 {
			t.Errorf("%s built with mode %o, committed with %o", rel, got[rel], want[rel])
		}
	}

	embedded := payloadUnder("impeccable/plugin")
	if len(embedded) != len(want) {
		t.Fatalf("payload embeds %d plugin files, the committed subtree holds %d", len(embedded), len(want))
	}
	modes := map[string]fs.FileMode{}
	for _, e := range must(manifestEntries()) {
		modes[e.Path] = e.Mode
	}
	for _, p := range embedded {
		rel := strings.TrimPrefix(p, "impeccable/plugin/")
		data, err := payloadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != readTestFile(t, filepath.Join(built, filepath.FromSlash(rel))) {
			t.Errorf("embedded %s differs from the built plugin subtree", p)
		}
		if modes[p]&0o111 != got[rel]&0o111 {
			t.Errorf("embedded %s has mode %o, built %o", p, modes[p], got[rel])
		}
	}
}
