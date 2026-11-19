package skills

import (
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const blueprintSkill = "---\nname: morning-brief\ndescription: Summarize unread email and calendar every morning.\nversion: 1.0.0\nmetadata:\n  icebath:\n    tags: [blueprint, email]\n    blueprint:\n      schedule: \"0 8 * * *\"\n      deliver: telegram\n      prompt: \"Summarize my unread email and today's calendar.\"\n---\nEvery morning, gather unread email and the day's calendar and send a digest.\n"

func TestParseBlueprint(t *testing.T) {
	spec, err := ParseBlueprint(blueprintSkill)
	if err != nil || spec == nil || spec.SkillName != "morning-brief" || spec.Schedule != "0 8 * * *" || spec.Deliver != "telegram" ||
		spec.Prompt == nil || !strings.HasPrefix(*spec.Prompt, "Summarize") {
		t.Fatalf("%+v %v", spec, err)
	}
	spec, _ = ParseBlueprint("---\nname: r\ndescription: d\nmetadata:\n  icebath:\n    blueprint:\n      schedule: \"every 1h\"\n---\n\nbody")
	if spec == nil || spec.Deliver != "origin" {
		t.Fatalf("%+v", spec)
	}
	if _, err := ParseBlueprint("---\nname: broken\nmetadata:\n  icebath:\n    blueprint:\n      deliver: origin\n---\n"); err == nil {
		t.Fatal("missing schedule accepted")
	}
	spec, err = ParseBlueprint("\uFEFF" + blueprintSkill)
	if err != nil || spec == nil || spec.Schedule != "0 8 * * *" {
		t.Fatalf("bom: %+v %v", spec, err)
	}
}

func TestBlueprintSpecForInstalled(t *testing.T) {
	h, _ := newTestHub(t)
	writeTestFile(t, filepath.Join(h.S.SkillsDir(), "productivity", "morning-brief", "SKILL.md"), blueprintSkill)
	writeTestFile(t, filepath.Join(h.S.SkillsDir(), "misc", "not-a-blueprint", "SKILL.md"), "---\nname: not-a-blueprint\ndescription: Just a regular skill.\nmetadata:\n  icebath:\n    tags: [misc]\n---\n")
	if spec, err := h.S.BlueprintSpecForInstalled("morning-brief"); err != nil || spec == nil || spec.Schedule != "0 8 * * *" {
		t.Fatalf("%+v %v", spec, err)
	}
	if spec, err := h.S.BlueprintSpecForInstalled("not-a-blueprint"); err != nil || spec != nil {
		t.Fatalf("%+v %v", spec, err)
	}
}

func TestLiveDashboardFrontmatterBlueprintIsAValidInstalledBlueprint(t *testing.T) {
	text, err := fs.ReadFile(OptionalFS(), "productivity/live-dashboard/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ParseBlueprint(string(text))
	if err != nil || spec == nil || spec.SkillName != "live-dashboard" {
		t.Fatalf("blueprint %+v %v", spec, err)
	}
	job := BlueprintToJobSpec(spec, "")
	skillsV, _ := job.Get("skills")
	schedule, _ := job.Get("schedule")
	prompt, _ := job.Get("prompt")
	if !reflect.DeepEqual(skillsV, []any{"live-dashboard"}) && !reflect.DeepEqual(skillsV, []string{"live-dashboard"}) {
		t.Fatalf("skills %#v", skillsV)
	}
	if len(strings.Fields(schedule.(string))) != 5 {
		t.Fatalf("invalid cron expr: %v", schedule)
	}
	if p, _ := prompt.(string); !strings.Contains(p, "[SILENT]") || strings.Contains(p, "~/.icebath") {
		t.Fatalf("prompt %q", p)
	}
}
