package skills

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestGetDisabledSkillsEmptyAndNull(t *testing.T) {
	if got := GetDisabledSkills(map[string]any{}, ""); len(got) != 0 {
		t.Fatalf("empty config %v", got)
	}
	for _, p := range []string{"", "telegram"} {
		if got := GetDisabledSkills(map[string]any{"skills": nil}, p); len(got) != 0 {
			t.Fatalf("null skills %q %v", p, got)
		}
	}
	cfg := map[string]any{"skills": map[string]any{"disabled": "solo", "platform_disabled": map[string]any{"telegram": []any{"tg", "icebath-agent"}}}}
	if got := GetDisabledSkills(cfg, "telegram"); !reflect.DeepEqual(got, map[string]bool{"solo": true, "tg": true}) {
		t.Fatalf("scalar and platform union %v", got)
	}
}

func TestSaveDisabledSkillsSortsAndDropsEssential(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	cfg := map[string]any{}
	if err := SaveDisabledSkills(context.Background(), h, cfg, map[string]bool{"skill-z": true, "skill-a": true, "icebath-agent": true}, ""); err != nil {
		t.Fatal(err)
	}
	if got := cfg["skills"].(map[string]any)["disabled"]; !reflect.DeepEqual(got, []any{"skill-a", "skill-z"}) {
		t.Fatalf("global save %v", got)
	}
	if err := SaveDisabledSkills(context.Background(), h, cfg, map[string]bool{"tg": true}, "telegram"); err != nil {
		t.Fatal(err)
	}
	pd := cfg["skills"].(map[string]any)["platform_disabled"].(map[string]any)
	if !reflect.DeepEqual(pd["telegram"], []any{"tg"}) {
		t.Fatalf("platform save %v", pd)
	}
	if _, err := os.Stat(filepath.Join(h, "config.yaml")); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

func TestToolSkillDisabled(t *testing.T) {
	s := NewScope(t.TempDir(), config.Tree{"skills": map[string]any{
		"disabled": []any{"a/one"}, "platform_disabled": map[string]any{"telegram": []any{"tg-skill"}, "discord": []any{"discord-skill"}}}})
	if !s.toolSkillDisabled("telegram", "tg-skill") || !s.toolSkillDisabled("telegram", "other", "tg-skill") ||
		!s.toolSkillDisabled("cli", "dup-demo", "a/one") || s.toolSkillDisabled("telegram", "other", "x") {
		t.Fatal("explicit platform checks")
	}
	s.Platform = "discord"
	if !s.toolSkillDisabled("", "discord-skill") {
		t.Fatal("scope platform not consulted")
	}
	s.Config = config.Tree{"skills": map[string]any{"disabled": nil}}
	if s.toolSkillDisabled("", "x") {
		t.Fatal("null disabled list must read as not disabled")
	}
}

func TestEnvPlatformPrecedence(t *testing.T) {
	t.Setenv("ICEBATH_SESSION_PLATFORM", "discord")
	t.Setenv("ICEBATH_PLATFORM", "")
	if got := envPlatform(); got != "discord" {
		t.Fatalf("session platform %q", got)
	}
	t.Setenv("ICEBATH_PLATFORM", "telegram")
	if got := envPlatform(); got != "telegram" {
		t.Fatalf("platform precedence %q", got)
	}
}

func TestFindAllSkillsExcludesDisabledAndCategories(t *testing.T) {
	h := t.TempDir()
	s := NewScope(h, config.Tree{"skills": map[string]any{"disabled": []any{"my-skill"}}})
	makeSkill(t, s.SkillsDir(), "my-skill")
	makeSkill(t, s.SkillsDir(), "coding/other-skill")
	ClearSkillsCache()
	for _, sk := range s.FindAllSkills(false) {
		if sk["name"] == "my-skill" {
			t.Fatal("disabled skill listed")
		}
	}
	all := s.FindAllSkills(true)
	if len(all) != 2 {
		t.Fatalf("skip disabled listing %v", all)
	}
	cats := skillCategories([]map[string]any{{"category": "mlops"}, {"category": "coding"}, {"category": "mlops"}})
	if !reflect.DeepEqual(cats, []string{"coding", "mlops"}) {
		t.Fatalf("categories %v", cats)
	}
	if got := skillCategories([]map[string]any{{"category": nil}}); got[0] != "uncategorized" {
		t.Fatalf("none category %v", got)
	}
}
