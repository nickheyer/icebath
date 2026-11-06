package skills

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const blueprintSkill = "---\nname: morning-brief\ndescription: Summarize unread email and calendar every morning.\nversion: 1.0.0\nmetadata:\n  icebath:\n    tags: [blueprint, email]\n    blueprint:\n      schedule: \"0 8 * * *\"\n      deliver: telegram\n      prompt: \"Summarize my unread email and today's calendar.\"\n---\nEvery morning, gather unread email and the day's calendar and send a digest.\n"

func TestParseBlueprint(t *testing.T) {
	spec, err := ParseBlueprint(blueprintSkill)
	if err != nil || spec == nil || spec.SkillName != "morning-brief" || spec.Schedule != "0 8 * * *" || spec.Deliver != "telegram" ||
		spec.Prompt == nil || !strings.HasPrefix(*spec.Prompt, "Summarize") {
		t.Fatalf("%+v %v", spec, err)
	}
	spec, _ = ParseBlueprint("---\nname: r\ndescription: d\nmetadata:\n  hermes:\n    blueprint:\n      schedule: \"every 1h\"\n---\n\nbody")
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

func TestCreateBlueprintJobBridgesToCreateJob(t *testing.T) {
	spec, _ := ParseBlueprint(blueprintSkill)
	prev := blueprintCreateJob
	t.Cleanup(func() { blueprintCreateJob = prev })
	var captured *pyjson.Object
	blueprintCreateJob = func(_ context.Context, js *pyjson.Object) (map[string]any, error) {
		captured = js
		return map[string]any{"id": "abc123"}, nil
	}
	job, err := CreateBlueprintJob(context.Background(), spec, map[string]any{"platform": "telegram"}, "")
	get := func(k string) any { v, _ := captured.Get(k); return v }
	if err != nil || job["id"] != "abc123" || get("schedule") != "0 8 * * *" || get("deliver") != "telegram" ||
		!strings.HasPrefix(get("prompt").(string), "Summarize") || get("skills").([]any)[0] != "morning-brief" || get("origin") == nil {
		t.Fatalf("%v %v", job, err)
	}
}

func TestExportBlueprintRoundTrips(t *testing.T) {
	md, err := ExportBlueprint(map[string]any{"name": "My Morning Brief", "schedule_display": "0 8 * * *", "skills": []any{"morning-brief"},
		"deliver": "telegram", "prompt": "Summarize my unread email."}, "# Morning Brief\n\nDoes the morning digest.", "")
	spec, perr := ParseBlueprint(md)
	if err != nil || perr != nil || spec == nil || spec.Schedule != "0 8 * * *" || spec.Deliver != "telegram" || spec.SkillName != "my-morning-brief" {
		t.Fatalf("%s %+v", md, spec)
	}
	job := map[string]any{"name": "poller", "schedule": map[string]any{"kind": "interval", "minutes": 30}, "skills": []any{"poller"}}
	for _, tc := range []struct {
		minutes int
		want    string
	}{{30, "every 30m"}, {120, "every 2h"}} {
		job["schedule"] = map[string]any{"kind": "interval", "minutes": tc.minutes}
		md, _ := ExportBlueprint(job, "body", "")
		if spec, _ := ParseBlueprint(md); spec == nil || spec.Schedule != tc.want {
			t.Fatalf("%s", md)
		}
	}
}
