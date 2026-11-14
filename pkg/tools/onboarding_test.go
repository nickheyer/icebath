package tools

import (
	"strings"
	"testing"
)

func onbTestFacts() OnboardingFacts {
	return OnboardingFacts{OS: "linux", Arch: "amd64", MachineKind: "computer", FactsSource: "local", Locale: "de-DE", Plugins: []string{"blender"}, Tours: true}
}

func TestOnboardingRecordLifecycle(t *testing.T) {
	facts := onbTestFacts()
	record := NewOnboardingRecord(facts, 1, "run")
	if OnboardingNextBeat(record) != "name" || !OnboardingActive(record) {
		t.Fatal("fresh record")
	}
	noPlugins := NewOnboardingRecord(OnboardingFacts{OS: "linux", Tours: false}, 1, "r")
	if onbBeats(noPlugins)["plugins"] != onbSkipped || onbBeats(noPlugins)["tour"] != onbSkipped {
		t.Fatal("unfit beats skipped")
	}
	step := OnboardingApply(record, OnbShown{Beat: "name", ToolCallID: "tc1"}, facts, 2)
	if len(step.Result) != 0 || step.Record["waiting"].(map[string]any)["beat"] != "name" || record["waiting"] != nil {
		t.Fatal("shown must wait and not mutate the input")
	}
	step = OnboardingApply(step.Record, OnbAnswered{Beat: "name", Answer: map[string]any{"name": "Ada", "label": "x"}}, facts, 3)
	if step.Reminder != "after_name" || step.Result["next"] != "theme" || onbPicks(step.Record)["name"] != "Ada" {
		t.Fatalf("%+v", step)
	}
	bad := OnboardingApply(step.Record, OnbAnswered{Beat: "theme", Answer: map[string]any{"layout": "x"}}, facts, 4)
	if !strings.Contains(bad.Result["error"].(string), "does not accept") {
		t.Fatal(bad.Result)
	}
	rec := OnboardingApply(step.Record, OnbRecorded{Beat: "theme", Value: "red"}, facts, 4)
	if !strings.Contains(rec.Result["error"].(string), "Apply the theme with set_theme") {
		t.Fatal(rec.Result)
	}
	applied := OnboardingApply(step.Record, OnbApplied{Tool: "set_theme", Args: map[string]any{}, Result: map[string]any{"applied": map[string]any{"accent": "red", "layout": "terminal-deck"}}}, facts, 5)
	if applied.Reminder != "after_layout" || onbBeats(applied.Record)["theme"] != onbDone || onbPicks(applied.Record)["layout"] != "terminal-deck" {
		t.Fatalf("%+v", applied)
	}
	repeat := OnboardingApply(applied.Record, OnbApplied{Tool: "set_theme", Args: map[string]any{}, Result: map[string]any{"applied": map[string]any{"accent": "blue"}}}, facts, 6)
	if repeat.Reminder != "already_theme" {
		t.Fatal(repeat.Reminder)
	}
	offer := OnboardingApply(applied.Record, OnbShown{Beat: "connectors"}, facts, 7)
	if offer.Reminder != "offer_first" || offer.Record["look_around"] != "offered" {
		t.Fatal(offer)
	}
	shown := OnboardingApply(offer.Record, OnbShown{Beat: "connectors", Suggest: []any{"github"}}, facts, 8)
	if len(shown.Result) != 0 {
		t.Fatal(shown.Result)
	}
	wrote := OnboardingApply(shown.Record, OnbWrote{Beat: "connectors", Present: map[string]any{"connected": []any{"github"}, "junk": 1}}, facts, 9)
	if wrote.Reminder != "wrote" || wrote.Result["wrote_instead"] != true || wrote.Result["junk"] != nil || wrote.Record["waiting"] != nil {
		t.Fatalf("%+v", wrote)
	}
	skipped := OnboardingApply(wrote.Record, OnbSkipped{Beat: "connectors"}, facts, 10)
	if skipped.Reminder != "moved_on" || onbBeats(skipped.Record)["connectors"] != onbSkipped {
		t.Fatal(skipped)
	}
	plugins := OnboardingApply(skipped.Record, OnbAnswered{Beat: "plugins", Answer: map[string]any{"installed": []any{"blender"}}}, facts, 11)
	if plugins.Reminder != "after_plugins" {
		t.Fatal(plugins.Reminder)
	}
	tour := OnboardingApply(plugins.Record, OnbRecorded{Beat: "tour", Value: "quick"}, facts, 12)
	if tour.Reminder != "tour_quick" || tour.Result["next"] != "build" {
		t.Fatalf("%+v", tour)
	}
	badTour := OnboardingApply(plugins.Record, OnbRecorded{Beat: "tour", Value: "later"}, facts, 12)
	if !strings.Contains(badTour.Result["error"].(string), "value for tour must be one of: quick, full, skip.") {
		t.Fatal(badTour.Result)
	}
	early := OnboardingApply(tour.Record, OnbDone{}, facts, 13)
	if !strings.Contains(early.Result["error"].(string), "Onboarding ends after the first build") {
		t.Fatal(early.Result)
	}
	badPicks := OnboardingApply(tour.Record, OnbShown{Beat: "build", Options: []any{map[string]any{"id": "icebath", "label": "x"}, map[string]any{"id": "games", "label": "y"}}}, facts, 14)
	errText := badPicks.Result["error"].(string)
	if !strings.Contains(errText, "icebath is added last by itself") || !strings.Contains(errText, "'games' is not an eligible id") || !strings.Contains(errText, "blender, brief, week, notes, explain") {
		t.Fatal(errText)
	}
	build := OnboardingApply(tour.Record, OnbShown{Beat: "build", Options: []any{map[string]any{"id": "blender", "label": " Make a scene "}}}, facts, 15)
	if len(build.Result) != 0 || onbPicks(build.Record)["build_options"].([]any)[0].(map[string]any)["label"] != "Make a scene" {
		t.Fatalf("%+v", build)
	}
	answered := OnboardingApply(build.Record, OnbAnswered{Beat: "build", Answer: map[string]any{"build": "blender", "label": "Make a scene"}}, facts, 16)
	if answered.Reminder != "build_blender" || answered.Result["completed"] != true || answered.Record["phase"] != onbPhaseDone || OnboardingActive(answered.Record) {
		t.Fatalf("%+v", answered)
	}
	done := OnboardingApply(answered.Record, OnbDone{}, facts, 17)
	if done.Reminder != "complete" || done.Result["completed"] != true {
		t.Fatal(done)
	}
	refused := OnboardingApply(answered.Record, OnbShown{Beat: "name"}, facts, 18)
	if refused.Result["error"] != onboardingEndedText {
		t.Fatal(refused.Result)
	}
	ended := OnboardingApply(tour.Record, OnbEnd{"skipped"}, facts, 19)
	if ended.Reminder != "skip_all" || ended.Record["phase"] != onbPhaseSkipped || ended.Record["ended_by"] != "skipped" || onbBeats(ended.Record)["build"] != onbSkipped {
		t.Fatal(ended)
	}
	if OnboardingApply(ended.Record, OnbEnd{"deleted"}, facts, 20).Result["skipped_all"] != true {
		t.Fatal("end on ended")
	}
}

func TestOnboardingTextRendering(t *testing.T) {
	if OnboardingUILanguage("zh_TW") != "zh-hant" || OnboardingUILanguage("DE-de") != "de" || OnboardingUILanguage("xx") != "en" || OnboardingUILanguage("") != "en" {
		t.Fatal("ui language")
	}
	if !strings.HasPrefix(OnboardingQuestion("name", "fr-FR"), "Comment") || OnboardingQuestion("tour", "fr") != "Want a look around first?" {
		t.Fatal("question")
	}
	facts := onbTestFacts()
	prompt := OnboardingOpeningPrompt(facts)
	if !strings.Contains(prompt, "linux on amd64, a computer.") || !strings.Contains(prompt, "Locale: de-DE.") || !strings.Contains(prompt, "mono, green, cyan") {
		t.Fatal(prompt)
	}
	record := NewOnboardingRecord(facts, 1, "r")
	onbPicks(record)["name"] = "Ada"
	if got := OnboardingReminder("after_name", record, facts, "", nil); !strings.Contains(got, "They go by Ada.") || !strings.Contains(got, `shows the name card`) {
		t.Fatal(got)
	}
	for _, beat := range []string{"name", "theme", "layout", "connectors", "plugins", "tour"} {
		onbBeats(record)[beat] = onbDone
	}
	next := OnboardingReminder("after_tour", record, facts, "", nil)
	if !strings.Contains(next, "shows the build card") || strings.Contains(next, "- blender") || !strings.Contains(next, "- notes (Turn my notes into a wiki)") {
		t.Fatal(next)
	}
	onbPicks(record)["installed"] = []any{"blender"}
	withBlender := OnboardingReminder("tour_skip", record, facts, "", nil)
	if strings.Contains(withBlender, "- games") || !strings.Contains(withBlender, "- blender (Build a 3D scene in Blender)") {
		t.Fatal(withBlender)
	}
	guidance := OnboardingBuildGuidance("brief", facts, func(name string) string {
		if name == "humanizer" {
			return ""
		}
		return name
	}, "Brief me")
	if !strings.Contains(guidance, `They picked "Brief me". Your job: write today's brief`) || !strings.Contains(guidance, "skill_view first: claude-design.") || !strings.Contains(guidance, "Never route around a connector") || !strings.Contains(guidance, "This Linux machine") {
		t.Fatal(guidance)
	}
	own := OnboardingBuildGuidance("own", OnboardingFacts{OS: "windows", Arch: "arm64", NvidiaPlatform: "rtx_spark", FactsSource: "remote"}, nil, "")
	if !strings.Contains(own, "Your job: build the task they named") || !strings.Contains(own, "x64 only") || !strings.Contains(own, "built for local models") || !strings.Contains(own, "another computer") || !strings.Contains(own, "does not reload by itself") {
		t.Fatal(own)
	}
	record["waiting"] = map[string]any{"beat": "theme"}
	wrote := OnboardingReminder("wrote", record, facts, "", nil)
	if !strings.Contains(wrote, "The theme card closed unanswered") || !strings.Contains(wrote, `"accent":"<colour>"`) {
		t.Fatal(wrote)
	}
	if note := OnboardingTurnNote(record); !strings.Contains(note, "the theme card is closed") {
		t.Fatal(note)
	}
	if got := OnboardingReminder("already_layout", record, facts, "", nil); !strings.Contains(got, "The layout was already set") {
		t.Fatal(got)
	}
	if OnboardingReminder("", record, facts, "", nil) != "" {
		t.Fatal("empty key")
	}
}

func TestFirstTaskPickErrors(t *testing.T) {
	facts := onbTestFacts()
	picks := map[string]any{}
	if got := firstTaskPickError(nil, facts, picks); !strings.Contains(got, "options is required.") {
		t.Fatal(got)
	}
	five := []any{}
	for i := 0; i < 5; i++ {
		five = append(five, map[string]any{"id": "notes", "label": "x"})
	}
	if got := firstTaskPickError(five, facts, picks); !strings.Contains(got, "pick at most 4.") {
		t.Fatal(got)
	}
	opts := []any{"bad", map[string]any{"id": "notes", "label": "ok"}, map[string]any{"id": "notes", "label": "again"}, map[string]any{"id": "site", "label": strings.Repeat("y", 33)}}
	got := firstTaskPickError(opts, facts, picks)
	for _, want := range []string{`each option is {"id": ..., "label": ...}`, "'notes' is picked twice", "is 33 characters; the limit is 32. Shorten it"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q missing %q", got, want)
		}
	}
	if firstTaskPickError([]any{map[string]any{"id": "explain", "label": "Explain"}}, facts, picks) != "" {
		t.Fatal("valid picks")
	}
	if got := firstTaskPickError([]any{map[string]any{"id": "notes", "label": " "}}, facts, picks); !strings.Contains(got, "the label for notes is empty") {
		t.Fatal(got)
	}
	skills := firstTaskSkills("watch", func(name string) string {
		if name == "competitor-news-monitor" {
			return ""
		}
		return name
	})
	if strings.Join(skills, "|") != "product-price-monitor|grounded-citations|humanizer" {
		t.Fatal(skills)
	}
}
