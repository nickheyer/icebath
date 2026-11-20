package skills

import (
	"path/filepath"
	"testing"
)

func TestNormalizeSkillLookupNameExported(t *testing.T) {
	h := t.TempDir()
	s := NewScope(h, nil)
	inside := filepath.Join(s.SkillsDir(), "research", "arxiv")
	outside := filepath.Join(t.TempDir(), "elsewhere", "skill")
	for in, want := range map[string]string{"": "", "  ": "", "research/arxiv": "research/arxiv",
		inside: filepath.Join("research", "arxiv"), "  " + inside + "  ": filepath.Join("research", "arxiv"), outside: outside} {
		if got := s.NormalizeSkillLookupName(in); got != want {
			t.Errorf("NormalizeSkillLookupName(%q) = %q want %q", in, got, want)
		}
	}
}
