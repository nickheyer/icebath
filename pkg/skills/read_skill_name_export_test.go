package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// The exported reader answers exactly as the package reader for a named, a nameless and a missing SKILL.md
func TestReadSkillNameExportedMatchesPackageReader(t *testing.T) {
	dir := t.TempDir()
	named := filepath.Join(dir, "named", "SKILL.md")
	nameless := filepath.Join(dir, "nameless", "SKILL.md")
	for p, body := range map[string]string{named: "---\nname: shipping-labels\ndescription: prints labels\n---\n# Labels\n", nameless: "---\ndescription: no name here\n---\n"} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{named, nameless, filepath.Join(dir, "missing", "SKILL.md")} {
		fallback := filepath.Base(filepath.Dir(p))
		if got, want := ReadSkillName(p, fallback), readSkillName(p, fallback); got != want {
			t.Fatalf("%s: exported %q differs from package %q", p, got, want)
		}
	}
	if got := ReadSkillName(named, "named"); got != "shipping-labels" {
		t.Fatalf("named skill read as %q", got)
	}
}
