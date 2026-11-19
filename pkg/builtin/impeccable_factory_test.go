package builtin

import (
	"slices"
	"testing"
)

func TestCreateTransformerFactoryShouldOnlyEmitAllowlistedFields(t *testing.T) {
	skill := sourceSkillOf(t, t.TempDir(), "---\nname: test\ndescription: Test\nargument-hint: \"[target]\"\nuser-invocable: true\nallowed-tools: Bash\n"+
		"license: MIT\ncompatibility: all\nmetadata: meta\ncontext: fork\n---\n\nBody")
	fm, body := parseImpeccableFrontmatter(skill["SKILL.md"])
	var keys []string
	for _, pair := range fm {
		keys = append(keys, pair.Key)
	}
	if !slices.Equal(keys, []string{"name", "description", "version", "license", "compatibility", "metadata"}) || body != "Body" {
		t.Fatalf("frontmatter keys %v, body %q", keys, body)
	}
	if fmLookup(fm, "license") != "MIT" || fmLookup(fm, "compatibility") != "all" || fmLookup(fm, "metadata") != "meta" {
		t.Fatalf("%+v", fm)
	}
}
