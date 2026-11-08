package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
)

func writeGatewaySkill(t *testing.T, root, name string) {
	t.Helper()
	d := filepath.Join(root, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: Description for " + name + ".\n---\n\n# " + name + "\n\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExternalDirSkillsIncludedInTelegramMenu(t *testing.T) {
	root := t.TempDir()
	h := filepath.Join(root, "home")
	local := filepath.Join(h, "skills")
	external := filepath.Join(root, "my-skills")
	lookalike := filepath.Join(root, "my-skills-extra")
	for _, d := range []string{local, external, lookalike} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(home.EnvHome, h)
	writeGatewaySkill(t, local, "local-one")
	writeGatewaySkill(t, external, "morning-briefing")
	writeGatewaySkill(t, lookalike, "lookalike-skill")
	hub := filepath.Join(local, ".hub")
	writeGatewaySkill(t, hub, "hub-only")
	cfg := config.Defaults()
	cfg["skills"] = map[string]any{"external_dirs": []any{external}}
	scope := skills.NewScope(h, cfg)
	scope.Cwd = root
	source := func(platform string) []GatewaySkill { return IterGatewaySkills(scope, platform) }
	menu, _ := TelegramMenuCommands(Default, cfg, source, 100)
	names := map[string]bool{}
	for _, e := range menu {
		names[e.Name] = true
	}
	if !names["local_one"] {
		t.Fatalf("local skill must appear: %v", menu)
	}
	if !names["morning_briefing"] {
		t.Fatalf("external skill from skills.external_dirs must appear: %v", menu)
	}
	if names["lookalike_skill"] {
		t.Fatalf("prefix-match sibling directories must not be admitted: %v", menu)
	}
	if names["hub_only"] {
		t.Fatalf("skills under .hub are never gateway commands: %v", menu)
	}
	for _, s := range source("telegram") {
		if s.Key == "/local-one" && (len(s.RelParts) != 1 || s.RelParts[0] != "local-one") {
			t.Fatalf("rel parts of a root level skill: %v", s.RelParts)
		}
	}
}

func TestGatewaySkillEntriesExportMatchesTheCollector(t *testing.T) {
	source := func(platform string) []GatewaySkill {
		return []GatewaySkill{{Key: "/alpha", Description: "A"}, {Key: "/beta", Description: "B"}}
	}
	entries, hidden := GatewaySkillEntries(NewRegistry(), "telegram", source, 1, map[string]bool{}, 40, SanitizeTelegramName)
	if hidden != 1 || len(entries) != 1 || entries[0].Name != "alpha" || entries[0].Description != "A" {
		t.Fatalf("entries %v hidden %d", entries, hidden)
	}
	if HomeSkillSource(t.TempDir(), nil)("telegram") != nil {
		t.Fatal("an empty home has no gateway skills")
	}
}
