package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
)

func TestSkillIndexFilesHookWalksLikeIterSkillIndexFiles(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"top", filepath.Join("cat", "nested"), filepath.Join("top", "references", "deep"), filepath.Join(".git", "hidden")} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
		os.WriteFile(filepath.Join(dir, d, "SKILL.md"), []byte("---\nname: x\n---\n"), 0o644)
	}
	got := observability.SkillIndexFilesHook(dir, "SKILL.md")
	want := IterSkillIndexFiles(dir, "SKILL.md")
	if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != 2 {
		t.Fatalf("%q %q", got, want)
	}
}

func TestBundledSkillTreesHookNamesBundledAndOptionalSkills(t *testing.T) {
	t.Setenv("ICEBATH_BUNDLED_SKILLS", "")
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", "")
	observability.ResetCatalogCaches()
	t.Cleanup(observability.ResetCatalogCaches)
	bundled, optional := observability.BundledSkillTreesHook()
	if _, err := bundled.Open("research/grounded-citations/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := optional.Open("yuanbao/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"grounded-citations", "yuanbao"} {
		if got := observability.SkillMetricName(name); got != name {
			t.Fatalf("%s read as %s", name, got)
		}
	}
	if got := observability.SkillMetricName("my-private-skill"); got != observability.Custom {
		t.Fatal(got)
	}
}
