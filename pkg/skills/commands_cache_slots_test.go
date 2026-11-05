package skills

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Identity of a published command map, the analogue of the Python is check
func commandsID(c SkillCommands) uintptr { return reflect.ValueOf(c).Pointer() }

// Number of published identity slots
func commandSlots() int {
	skillCmds.mu.Lock()
	defer skillCmds.mu.Unlock()
	return len(skillCmds.byKey)
}

// Scope over its own home with one skill under home/skills and a cwd outside any repo
func slotScope(t *testing.T, homeDir, platform string, skillNames ...string) *Scope {
	t.Helper()
	os.MkdirAll(filepath.Join(homeDir, "skills"), 0o755)
	os.WriteFile(filepath.Join(homeDir, "config.yaml"), []byte("{}\n"), 0o644)
	for _, n := range skillNames {
		makeCmdSkill(t, filepath.Join(homeDir, "skills"), n, "", "", "")
	}
	s := NewScope(homeDir, config.Defaults())
	s.Platform = platform
	s.Cwd = t.TempDir()
	return s
}

func TestCommandCacheEmptyPlatformSharesOneSlot(t *testing.T) {
	cmdScope(t)
	s := slotScope(t, filepath.Join(t.TempDir(), "home"), "", "test-skill")
	first := s.GetSkillCommands()
	if _, ok := first["/test-skill"]; !ok {
		t.Fatalf("missing command %v", first)
	}
	again := *s
	again.Platform = ""
	if commandsID(again.GetSkillCommands()) != commandsID(first) || commandSlots() != 1 {
		t.Fatalf("empty platform rescanned into %d slots", commandSlots())
	}
}

func TestCommandCacheSlotsPerPlatformAndHome(t *testing.T) {
	cmdScope(t)
	root := t.TempDir()
	a := slotScope(t, filepath.Join(root, "profile_a"), "telegram", "a-skill")
	b := slotScope(t, filepath.Join(root, "profile_b"), "telegram", "b-skill")
	aTelegram := a.GetSkillCommands()
	if _, ok := aTelegram["/a-skill"]; !ok || commandSlots() != 1 {
		t.Fatalf("first slot %v", aTelegram)
	}
	if _, ok := b.GetSkillCommands()["/b-skill"]; !ok || commandSlots() != 2 {
		t.Fatal("home did not open a second slot")
	}
	aDiscord := *a
	aDiscord.Platform = "discord"
	if _, ok := aDiscord.GetSkillCommands()["/a-skill"]; !ok || commandSlots() != 3 {
		t.Fatal("platform did not open a third slot")
	}
	if commandsID(a.GetSkillCommands()) != commandsID(aTelegram) || commandSlots() != 3 {
		t.Fatal("revisited slot rescanned")
	}
}

func TestCommandCacheKeysProjectAsThirdDimension(t *testing.T) {
	cmdScope(t)
	root := t.TempDir()
	repoA, repoB := filepath.Join(root, "repo_a"), filepath.Join(root, "repo_b")
	for _, repo := range []string{repoA, repoB} {
		os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	}
	makeCmdSkill(t, filepath.Join(repoA, ".icebath", "skills"), "proj-a-skill", "", "", "")
	makeCmdSkill(t, filepath.Join(repoB, ".icebath", "skills"), "proj-b-skill", "", "", "")
	s := slotScope(t, filepath.Join(root, "shared-home"), "")
	if err := s.Config.Set("skills.trusted_project_dirs", []any{repoA, repoB}); err != nil {
		t.Fatal(err)
	}
	s.Cwd = repoA
	cmdsA := s.GetSkillCommands()
	_, hasA := cmdsA["/proj-a-skill"]
	_, leakB := cmdsA["/proj-b-skill"]
	if !hasA || leakB || commandSlots() != 1 {
		t.Fatalf("project a view %v", cmdsA)
	}
	inB := *s
	inB.Cwd = repoB
	cmdsB := inB.GetSkillCommands()
	_, hasB := cmdsB["/proj-b-skill"]
	_, leakA := cmdsB["/proj-a-skill"]
	if !hasB || leakA || commandSlots() != 2 {
		t.Fatalf("project b view %v", cmdsB)
	}
	if commandsID(s.GetSkillCommands()) != commandsID(cmdsA) || commandsID(inB.GetSkillCommands()) != commandsID(cmdsB) || commandSlots() != 2 {
		t.Fatal("project slots rescanned")
	}
}

func TestReloadInvalidatesEveryIdentitySlot(t *testing.T) {
	cmdScope(t)
	root := t.TempDir()
	a := slotScope(t, filepath.Join(root, "profile_a"), "", "a-skill")
	b := slotScope(t, filepath.Join(root, "profile_b"), "", "b-skill")
	if _, ok := a.GetSkillCommands()["/a-skill"]; !ok {
		t.Fatal("a view")
	}
	if _, ok := b.GetSkillCommands()["/b-skill"]; !ok || commandSlots() != 2 {
		t.Fatal("b view")
	}
	makeCmdSkill(t, filepath.Join(a.Home, "skills"), "a-second-skill", "", "", "")
	result, err := a.ReloadSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if added, _ := result["added"].([]map[string]any); len(added) == 0 {
		if anyAdded, _ := result["added"].([]any); len(anyAdded) == 0 {
			t.Fatalf("reload reported nothing added %v", result)
		}
	}
	if commandSlots() != 1 {
		t.Fatalf("reload kept %d slots", commandSlots())
	}
	skillCmds.mu.Lock()
	var only SkillCommands
	for _, v := range skillCmds.byKey {
		only = v
	}
	skillCmds.mu.Unlock()
	_, first := only["/a-skill"]
	_, second := only["/a-second-skill"]
	if !first || !second {
		t.Fatalf("reloaded slot %v", only)
	}
	if _, ok := b.GetSkillCommands()["/b-skill"]; !ok {
		t.Fatal("b rescan lost its view")
	}
}
