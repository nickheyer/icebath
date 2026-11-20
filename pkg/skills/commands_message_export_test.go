package skills

import (
	"context"
	"testing"
)

func TestBuildSkillMessageExportMatchesTheBuilder(t *testing.T) {
	s, dir := usageHome(t)
	skillDir := writeUsageSkill(t, dir, "demo", "tools")
	loaded := map[string]any{"content": "Body for ${ICEBATH_SKILL_DIR}", "setup_skipped": true}
	got := s.BuildSkillMessage(context.Background(), loaded, skillDir, "[activation]", "do it", "late", "sess")
	want := s.buildSkillMessage(context.Background(), loaded, skillDir, "[activation]", "do it", "late", "sess")
	if got == "" || got != want {
		t.Fatalf("export diverges from the builder:\n%s\n---\n%s", got, want)
	}
}
