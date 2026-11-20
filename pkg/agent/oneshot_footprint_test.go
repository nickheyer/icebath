package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

func oneshotSkillsDir(t *testing.T, tmp string) string {
	t.Helper()
	writeFile(t, filepath.Join(tmp, "skills", "misc", "demo-skill", "SKILL.md"), "---\nname: demo-skill\ndescription: Demo skill for tests.\n---\n# Demo\n")
	return filepath.Join(tmp, "skills")
}

func oneshotSkillsPrompt(t *testing.T, h, dir string, names ...string) string {
	t.Helper()
	avail := map[string]bool{}
	for _, n := range names {
		avail[n] = true
	}
	out, err := skills.NewScope(h, nil).BuildSkillsSystemPrompt(context.Background(), skills.SkillsPromptOptions{AvailableTools: avail, SkillsDirOverride: dir})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOneshotSkillsPromptDropsAuthoringCoaching(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	t.Setenv("ICEBATH_HOME", h)
	dir := oneshotSkillsDir(t, tmp)
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "")
	interactive := oneshotSkillsPrompt(t, h, dir, "skill_view", "skills_list", "skill_manage")
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	prompt := oneshotSkillsPrompt(t, h, dir, "skill_view", "skills_list")
	if !strings.Contains(prompt, "demo-skill") || !strings.Contains(prompt, "skill_view") {
		t.Fatalf("oneshot prompt %q", prompt)
	}
	if strings.Contains(prompt, "skill_manage") || strings.Contains(prompt, "offer to save as a skill") {
		t.Fatalf("oneshot prompt coaches authoring %q", prompt)
	}
	if !strings.Contains(interactive, "skill_manage") || !strings.Contains(interactive, "offer to save as a skill") {
		t.Fatalf("interactive prompt %q", interactive)
	}
}

func TestAgentIsOneshotFreezesAtFirstUse(t *testing.T) {
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	a := &AIAgent{}
	if !a.agentIsOneshot(context.Background()) {
		t.Fatal("single query marker not read")
	}
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "")
	if !a.agentIsOneshot(context.Background()) {
		t.Fatal("frozen fact changed after the marker cleared")
	}
	if v, pinned := a.oneshotSession.Pinned(); !v || !pinned {
		t.Fatal("pin not visible to delegation readers")
	}
	prev := tools.ActivePolicySource
	tools.ActivePolicySource = func(context.Context) (string, bool) { return "cli", true }
	defer func() { tools.ActivePolicySource = prev }()
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	if tools.IsSingleQuerySession(context.Background(), nil) {
		t.Fatal("interactive gateway policy must win over a stray env marker")
	}
}
