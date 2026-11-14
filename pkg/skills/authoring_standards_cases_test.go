package skills

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var authoringMarketing = regexp.MustCompile(`(?i)\b(powerful|comprehensive|seamless|revolutionary|cutting-edge|state-of-the-art)\b`)

// Every SKILL.md of the bundled and optional trees keyed by its tree and directory
func shippedSkillFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for tree, root := range map[string]fs.FS{"skills": BundledFS(), "optional-skills": OptionalFS()} {
		err := fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
				return err
			}
			raw, err := fs.ReadFile(root, path)
			if err != nil {
				return err
			}
			rel := tree
			if dir := strings.TrimSuffix(path, "/SKILL.md"); dir != path {
				rel += "/" + dir
			}
			out[rel] = string(raw)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// Frontmatter mapping of a shipped SKILL.md, failing on a missing opener or closer
func authoringFrontmatter(t *testing.T, rel, content string) map[string]any {
	t.Helper()
	if !strings.HasPrefix(content, "---") {
		t.Fatalf("%s: SKILL.md must start with ---", rel)
	}
	loc := regexp.MustCompile(`\n---\s*\n`).FindStringIndex(content[3:])
	if loc == nil {
		t.Fatalf("%s: unclosed frontmatter", rel)
	}
	fm, _ := ParseFrontmatter(content)
	if fm == nil {
		t.Fatalf("%s: frontmatter must be a YAML mapping", rel)
	}
	return fm
}

func TestDescriptionHardline(t *testing.T) {
	files := shippedSkillFiles(t)
	if len(files) == 0 {
		t.Fatal("no shipped SKILL.md found")
	}
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		t.Run(rel, func(t *testing.T) {
			fm := authoringFrontmatter(t, rel, files[rel])
			desc := ""
			if pyrepr.Truthy(fm["description"]) {
				desc = pyrepr.Str(fm["description"])
			}
			if n := len([]rune(desc)); n > SkillPromptDescLimit {
				t.Errorf("%s: description %d chars (hardline %d)", rel, n, SkillPromptDescLimit)
			}
			if !strings.HasSuffix(pystr.Rstrip(desc), ".") {
				t.Errorf("%s: description must end with a period", rel)
			}
			if m := authoringMarketing.FindString(desc); m != "" {
				t.Errorf("%s: marketing word in description: %s", rel, pyrepr.Repr(m))
			}
		})
	}
}
