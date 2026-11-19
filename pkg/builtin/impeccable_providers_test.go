package builtin

import (
	"slices"
	"strings"
	"testing"
)

func TestProviderShouldNotEmitFieldsOutsideAllowlist(t *testing.T) {
	allOptionalFields := []string{"user-invocable", "argument-hint", "license", "compatibility", "metadata", "allowed-tools"}
	allowlist := []string{"license", "compatibility", "metadata"}
	source, err := payloadFile(impeccableSkillRoot + "/SKILL.src.md")
	if err != nil {
		t.Fatal(err)
	}
	sourceFm, _ := parseImpeccableFrontmatter(string(source))
	content := renderedSkill(t)["SKILL.md"]
	var frontmatterLines []string
	inFrontmatter := false
	for _, line := range strings.Split(content, "\n") {
		if line == "---" {
			if inFrontmatter {
				break
			}
			inFrontmatter = true
			continue
		}
		if inFrontmatter {
			frontmatterLines = append(frontmatterLines, line)
		}
	}
	for _, field := range allOptionalFields {
		if slices.Contains(allowlist, field) {
			continue
		}
		if fmLookup(sourceFm, field) == nil {
			t.Errorf("the skill source declares no %s", field)
		}
		if slices.ContainsFunc(frontmatterLines, func(l string) bool { return strings.HasPrefix(l, field+":") }) {
			t.Errorf("%s emitted outside the allowlist\n%s", field, content)
		}
	}
}
