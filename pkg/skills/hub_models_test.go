package skills

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Accepted referenced paths sorted, failing the test on a urlsplit error
func refPaths(t *testing.T, md string) ([]string, bool) {
	t.Helper()
	paths, ok, err := referencedSupportPaths(md)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(paths)), ok
}

func TestSupportPathExtractionIgnoresGlobShapedProse(t *testing.T) {
	for _, glob := range []string{
		"references/*.md", "references/file?", "references/file?.md", "references/agent.v?.md", "references/file?x.md",
		"references/file?[ab].md", "references/file??.md", "references/[ab].md", "references/%2A.md", "references/file%3F.md",
		"references/%5Bab%5D.md",
	} {
		md := "\nSee [the web reference](references/web.md) for details.\nComplex cases are documented under `" + glob + "`.\n"
		if got, ok := refPaths(t, md); !ok || !reflect.DeepEqual(got, []string{"references/web.md"}) {
			t.Fatal(glob, got)
		}
	}
}

func TestSupportPathExtractionPreservesConcreteURLSuffixes(t *testing.T) {
	for ref, want := range map[string]string{
		"templates/report.md?raw=1": "templates/report.md", "references/LICENSE?download": "references/LICENSE",
		"references/guide.md?view": "references/guide.md", "references/guide.md?preview-mode": "references/guide.md",
		"references/guide.md?view&inline&theme=dark": "references/guide.md", "references/LICENSE?plain&download=1": "references/LICENSE",
		"references/LICENSE?.well-known=1": "references/LICENSE", "references/guide.md#usage": "references/guide.md",
		"references/my%20guide.md": "references/my guide.md", "references/foo%23bar.md": "references/foo#bar.md",
	} {
		if got, ok := refPaths(t, "Use `"+ref+"`."); !ok || !reflect.DeepEqual(got, []string{want}) {
			t.Fatal(ref, got)
		}
	}
}

func TestReferencedSupportPathsIgnoresGlobsPlaceholdersAndTruncatedTokens(t *testing.T) {
	md := "Load `references/type-*.md` before drawing, and `references/type-<name>.md` for the matching type.\n" +
		"Real files: [flowchart](references/type-flowchart.md) and `references/type-architecture.md`.\n"
	if got, _ := refPaths(t, md); !reflect.DeepEqual(got, []string{"references/type-architecture.md", "references/type-flowchart.md"}) {
		t.Fatal(got)
	}
}

func TestReferencedSupportPathsKeepsRealBacktickReferences(t *testing.T) {
	md := "Run `python3 scripts/self_check.py <file>` and see `references/guide.md`.\n"
	if got, _ := refPaths(t, md); !reflect.DeepEqual(got, []string{"references/guide.md", "scripts/self_check.py"}) {
		t.Fatal(got)
	}
}

func TestReferencedSupportPathsSameDirLinks(t *testing.T) {
	for md, want := range map[string][]string{
		"---\nname: x\ndescription: x\n---\nsee [notes](NOTES) and `README`\n":                      {},
		"---\nname: x\ndescription: x\n---\n[a](CONTEXT-FORMAT.md?raw=1) [b](DEEPENING.md#usage)\n": {"CONTEXT-FORMAT.md", "DEEPENING.md"},
		"---\nname: x\ndescription: x\n---\n[home](skill.md)\n":                                     {},
		"---\nname: x\ndescription: x\n---\n[a](A.md) [a2](a.md)\n":                                 {},
		"[up](../../tools/REGISTRY.md) [ok](./DEFS.md) [web](https://x.dev/a.md) [m](mailto:a@b.c)": {"DEFS.md"},
	} {
		if got, ok := refPaths(t, md); !ok || !reflect.DeepEqual(got, want) && len(got)+len(want) > 0 {
			t.Fatal(md, got)
		}
	}
	if _, ok := refPaths(t, "see `references/../secrets`"); ok {
		t.Fatal("traversal must fail closed")
	}
}

func TestNormalizeBundlePathRejectsColonAnywhere(t *testing.T) {
	for _, bad := range []string{"scripts/helper.py:payload", "scripts/a:b.py", "a:b/scripts/helper.py", "scripts/helper.py:", "C:", "C:/Windows/System32"} {
		if _, err := normalizeBundlePath(bad, "bundle file path", true); err == nil || !strings.HasPrefix(err.Error(), "Unsafe bundle file path") {
			t.Fatal(bad, err)
		}
	}
	for _, good := range []string{"scripts/helper.py", "assets/data/sample.wav"} {
		if got, err := normalizeBundlePath(good, "bundle file path", true); err != nil || got != good {
			t.Fatal(good, got, err)
		}
	}
	if got, err := normalizeBundlePath(" .\\refs\\./a.md ", "bundle file path", true); err != nil || got != "refs/a.md" {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"", "  ", "/abs", "a/../b", "."} {
		if _, err := normalizeBundlePath(bad, "x", true); err == nil {
			t.Fatal(bad)
		}
	}
	if _, err := validateHubSkillName("cat/skill"); err == nil || err.Error() != "Unsafe skill name: cat/skill" {
		t.Fatal(err)
	}
	if got, err := normalizeLockInstallPath("mlops/training/vllm", "vllm"); err != nil || got != "mlops/training/vllm" {
		t.Fatal(got, err)
	}
	if _, err := normalizeLockInstallPath("mlops/other", "vllm"); err == nil || err.Error() != "Unsafe install path: mlops/other" {
		t.Fatal(err)
	}
}

func TestResolveLockInstallPathRefusesRedirectsRootAndEscapes(t *testing.T) {
	h := t.TempDir()
	s := NewScope(h, config0())
	writeRel(t, s.SkillsDir(), "cat/ok/SKILL.md", "x")
	got, err := resolveLockInstallPath(s, "cat/ok", "ok")
	if err != nil || got != filepath.Join(resolvePath(s.SkillsDir()), "cat", "ok") {
		t.Fatal(got, err)
	}
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(s.SkillsDir(), "link"))
	os.Symlink(outside, filepath.Join(s.SkillsDir(), "cat", "lnk"))
	for _, c := range [][2]string{{"link/x", "x"}, {"cat/lnk", "lnk"}, {".", "skills"}, {"../x", "x"}, {"", "x"}} {
		if _, err := resolveLockInstallPath(s, c[0], c[1]); err == nil {
			t.Fatal(c)
		}
	}
	if _, err := resolveLockInstallPath(s, "link", "link"); err == nil || err.Error() != "Unsafe install path: link" {
		t.Fatal(err)
	}
}

func TestHubModelHelpers(t *testing.T) {
	b := &SkillBundle{Source: "github", Identifier: "openai/skills/skill-creator"}
	if got := sourceURLForBundle(b); got != "https://github.com/openai/skills/tree/main/skill-creator" {
		t.Fatal(got)
	}
	b.Identifier = "openai/skills"
	if got := sourceURLForBundle(b); got != "https://github.com/openai/skills" {
		t.Fatal(got)
	}
	metas := dedupeByTrust([]SkillMeta{{Identifier: "a", TrustLevel: "community", Name: "1"}, {Identifier: "b"}, {Identifier: "a", TrustLevel: "trusted", Name: "2"}, {Identifier: "a", TrustLevel: "trusted", Name: "3"}})
	if len(metas) != 2 || metas[0].Name != "2" || metas[1].Identifier != "b" {
		t.Fatal(metas)
	}
	if !matchesQuery("pdf tools", "PDF", []any{"tools", 3}) || matchesQuery("zzz", "a", []string{"b"}) {
		t.Fatal("query match")
	}
	hits := firstMatching("a", []string{"ab", "cd", "ca", "aa"}, func(s string) []any { return []any{s} }, func(s string) *SkillMeta { return &SkillMeta{Name: s} }, 2)
	if len(hits) != 2 || hits[1].Name != "ca" {
		t.Fatal(hits)
	}
	fm := hubParseFrontmatter("\xEF\xBB\xBF---\nname: x\nmetadata:\n  hermes:\n    tags: [a, b]\n---\nbody")
	if fm["name"] != "x" || !reflect.DeepEqual(hubTags(fm), []any{"a", "b"}) || len(hubParseFrontmatter("no fm")) != 0 || len(hubParseFrontmatter("---\n: [\n---\n")) != 0 {
		t.Fatal(fm)
	}
	repo := "o/r"
	d := skillMetaToDict(SkillMeta{Name: "n", Repo: &repo, Tags: []string{"t"}})
	if !reflect.DeepEqual(d.Keys, []string{"name", "description", "source", "identifier", "trust_level", "repo", "path", "tags", "extra"}) {
		t.Fatal(d.Keys)
	}
	if src := (SourceBase{ID: "x"}); src.TrustLevelFor("y") != "community" || src.SourceID() != "x" {
		t.Fatal(src)
	}
}

func TestBuiltInRestoreIntoALeftoverDirNeverWritesThroughLinksOrOverUserFiles(t *testing.T) {
	t.Setenv("ICEBATH_BUNDLED_SKILLS", "")
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", "")
	tmp := t.TempDir()
	s := NewScope(filepath.Join(tmp, "home"), nil)
	var srcDir string
	for _, b := range discoverBundledSkills(bundledTree()) {
		if b.name == "arxiv" {
			srcDir = b.dir
		}
	}
	dest := filepath.Join(s.SkillsDir(), path.Base(path.Dir(srcDir)), "arxiv")
	victim := writeRel(t, tmp, "victim.txt", "USER SECRET\n")
	os.MkdirAll(filepath.Join(dest, "scripts"), 0o755)
	os.Symlink(victim, filepath.Join(dest, "scripts", "search_arxiv.py"))
	ctx := context.Background()
	if r, err := s.EnsureBundledSkill(ctx, "arxiv"); err != nil || r["action"] != "not_restored" {
		t.Fatal(r, err)
	}
	if mustRead(t, victim) != "USER SECRET\n" || lexists(filepath.Join(dest, "SKILL.md")) {
		t.Fatal("restore wrote through a link")
	}
	os.Remove(filepath.Join(dest, "scripts", "search_arxiv.py"))
	writeRel(t, dest, "scripts/search_arxiv.py", "# my local edits\n")
	if r, err := s.EnsureBundledSkill(ctx, "arxiv"); err != nil || r["action"] != "restored" {
		t.Fatal(r, err)
	}
	want, _ := fs.ReadFile(BundledFS(), path.Join(srcDir, "SKILL.md"))
	if mustRead(t, filepath.Join(dest, "scripts", "search_arxiv.py")) != "# my local edits\n" || mustRead(t, filepath.Join(dest, "SKILL.md")) != string(want) {
		t.Fatal("restore clobbered a user file")
	}
	writeRel(t, dest, "SKILL.md", "---\nname: my-own-thing\ndescription: x\n---\nmine\n")
	if r, err := s.EnsureBundledSkill(ctx, "arxiv"); err != nil || r["action"] != "not_restored" || !strings.Contains(mustRead(t, filepath.Join(dest, "SKILL.md")), "my-own-thing") {
		t.Fatal(r, err)
	}
}
