package cron

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
)

func mustBlueprint(t *testing.T, key string) *AutomationBlueprint {
	t.Helper()
	b := getBlueprint(context.Background(), key)
	if b == nil {
		t.Fatalf("no blueprint %s", key)
	}
	return b
}

func mustFill(t *testing.T, key string, values map[string]any) *pyjson.Object {
	t.Helper()
	spec, err := fillBlueprint(mustBlueprint(t, key), values, nil)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func specStr(spec *pyjson.Object, key string) string {
	v, _ := spec.Get(key)
	s, _ := v.(string)
	return s
}

func fillErr(t *testing.T, key string, values map[string]any, want string) {
	t.Helper()
	_, err := fillBlueprint(mustBlueprint(t, key), values, nil)
	var fe *BlueprintFillError
	if !errors.As(err, &fe) || !strings.Contains(fe.Msg, want) {
		t.Fatalf("fill %s %v error %v want %q", key, values, err, want)
	}
}

func cronGaps(t *testing.T, expr string, start time.Time, n int) map[time.Duration]bool {
	t.Helper()
	e, err := ParseCron(expr)
	if err != nil {
		t.Fatal(err)
	}
	var times []time.Time
	for cur := start; len(times) < n; {
		if cur, err = e.Next(cur); err != nil {
			t.Fatal(err)
		}
		times = append(times, cur)
	}
	gaps := map[time.Duration]bool{}
	for i := 1; i < len(times); i++ {
		gaps[times[i].Sub(times[i-1])] = true
	}
	return gaps
}

func TestBlueprintBadSlotTypeRejected(t *testing.T) {
	if _, err := NewBlueprintSlot(BlueprintSlot{Name: "x", Type: "bogus", Label: "X"}); err == nil || err.Error() != "unknown slot type 'bogus' (slot x)" {
		t.Fatalf("err %v", err)
	}
}

func TestBlueprintScheduleResolution(t *testing.T) {
	cases := []struct {
		key    string
		values map[string]any
		want   string
	}{
		{"morning-brief", map[string]any{"time": "08:30"}, "30 8 * * *"},
		{"important-mail", map[string]any{"interval_min": "15", "criteria": "x", "deliver": "origin"}, "*/15 * * * *"},
		{"weekly-review", map[string]any{"time": "18:00", "day": "sunday", "deliver": "origin"}, "0 18 * * 0"},
		{"custom-reminder", map[string]any{"what": "stretch", "time": "14:00", "recurrence": "weekdays", "deliver": "origin"}, "0 14 * * 1-5"},
	}
	for _, c := range cases {
		if got := specStr(mustFill(t, c.key, c.values), "schedule"); got != c.want {
			t.Fatalf("%s schedule %q want %q", c.key, got, c.want)
		}
	}
}

func TestBlueprintValidation(t *testing.T) {
	fillErr(t, "morning-brief", map[string]any{"time": "25:99"}, "invalid time")
	if got := specStr(mustFill(t, "morning-brief", map[string]any{"time": "08:00", "deliver": "slack"}), "deliver"); got != "slack" {
		t.Fatalf("deliver %q", got)
	}
	fillErr(t, "morning-brief", map[string]any{"tiem": "07:15"}, "unknown slot")
	spec := mustFill(t, "hydration-move", map[string]any{"interval_hours": "2"})
	gaps := cronGaps(t, specStr(spec, "schedule"), time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC), 3)
	if len(gaps) != 1 || !gaps[2*time.Hour] {
		t.Fatalf("expected 2h gaps, got %s -> %v", specStr(spec, "schedule"), gaps)
	}
	if p := specStr(mustFill(t, "important-mail", map[string]any{"interval_min": "30", "criteria": "from my CEO", "deliver": "origin"}), "prompt"); !strings.Contains(p, "from my CEO") {
		t.Fatalf("prompt %q", p)
	}
	origin := map[string]any{"platform": "telegram", "chat_id": "9"}
	spec, err := fillBlueprint(mustBlueprint(t, "morning-brief"), map[string]any{"time": "08:00"}, origin)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := spec.Get("origin"); !pyrepr.Equal(got, origin) {
		t.Fatalf("origin %#v", got)
	}
}

func TestBlueprintDeeplinkShape(t *testing.T) {
	url := blueprintDeeplink(mustBlueprint(t, "morning-brief"), map[string]any{"time": "07:15"})
	if !strings.HasPrefix(url, "icebath://blueprint/morning-brief?") || !strings.Contains(url, "time=07") {
		t.Fatalf("url %q", url)
	}
}

func TestBlueprintDocsIndexEntries(t *testing.T) {
	var index []any
	for i := range blueprintCatalog {
		index = append(index, blueprintCatalogEntry(&blueprintCatalog[i]))
	}
	if len(index) != len(blueprintCatalog) {
		t.Fatalf("index %d", len(index))
	}
	if _, err := pyjson.Dumps(index); err != nil {
		t.Fatal(err)
	}
	for _, e := range index {
		o := e.(*pyjson.Object)
		_, hasCommand := o.Get("command")
		_, hasURL := o.Get("appUrl")
		if !hasCommand || !hasURL {
			t.Fatalf("entry %v", o.Keys)
		}
	}
}

func TestPriceWatchBlueprintScheduleResolves(t *testing.T) {
	bp := mustBlueprint(t, "price-watch")
	slot := blueprintSlot(bp, "", "interval_h")
	for _, opt := range slot.Options {
		expr, err := pyrepr.FormatMap(bp.ScheduleTemplate, map[string]any{"interval_h": opt})
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(expr)
		if len(fields) != 5 || fields[1] != "*/"+pyrepr.Str(opt) {
			t.Fatalf("invalid cron expr: %s", expr)
		}
	}
}

func TestTaskBlueprintLoadsProcedure(t *testing.T) {
	for _, c := range [][2]string{{"morning-brief", "google-workspace"}, {"important-mail", "email-inbox-triage"}, {"weekly-review", "weekly-review-planning"}, {"price-watch", "product-price-monitor"}} {
		b := mustBlueprint(t, c[0])
		if !containsString(b.Skills, c[1]) {
			t.Fatalf("%s skills %v", c[0], b.Skills)
		}
		if c[0] == "weekly-review" {
			spec, err := fillBlueprint(b, map[string]any{}, nil)
			if err != nil || !strings.Contains(specStr(spec, "prompt"), c[1]) {
				t.Fatalf("weekly review prompt %v", err)
			}
		}
	}
}

// Names of bundled skill directories reached at one or two category levels
func bundledSkillDirs(t *testing.T) (all, shallow map[string]bool) {
	all, shallow = map[string]bool{}, map[string]bool{}
	err := fs.WalkDir(skills.BundledFS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
			return err
		}
		dir := path.Dir(p)
		all[path.Base(dir)] = true
		if depth := strings.Count(dir, "/"); depth == 1 || depth == 2 {
			shallow[path.Base(dir)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return all, shallow
}

func TestEveryBlueprintSkillIsBundledAndResolves(t *testing.T) {
	all, shallow := bundledSkillDirs(t)
	for _, b := range blueprintCatalog {
		for _, s := range b.Skills {
			if !all[s] {
				t.Fatalf("%s skill %s not bundled", b.Key, s)
			}
			if !shallow[s] {
				t.Fatalf("blueprint %s loads nonexistent skill %s", pyrepr.StrRepr(b.Key), pyrepr.StrRepr(s))
			}
		}
	}
}

func TestPriceWatchFillsAndPersistsWithChosenCadence(t *testing.T) {
	bp := mustBlueprint(t, "price-watch")
	for _, hours := range blueprintSlot(bp, "", "interval_h").Options {
		s, _ := newTestStore(t)
		spec, err := fillBlueprint(bp, map[string]any{"item": "https://example.test/widget", "condition": "below 42 credits", "interval_h": hours, "deliver": "local"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		opts, err := createJobOptionsFromSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		created, err := s.createJob(opts)
		if err != nil {
			t.Fatal(err)
		}
		saved, err := s.getJob(created.ID)
		if err != nil || saved == nil {
			t.Fatalf("saved %v %v", saved, err)
		}
		if !containsString(saved.Skills, "product-price-monitor") || saved.Deliver == nil || *saved.Deliver != "local" {
			t.Fatalf("saved %v %v", saved.Skills, saved.Deliver)
		}
		for _, text := range []string{"https://example.test/widget", "below 42 credits", "[SILENT]"} {
			if !strings.Contains(saved.Prompt, text) {
				t.Fatalf("prompt lacks %s", text)
			}
		}
		h, _ := strconv.Atoi(pyrepr.Str(hours))
		gaps := cronGaps(t, saved.Schedule.Expr, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 26)
		if len(gaps) != 1 || !gaps[time.Duration(h)*time.Hour] {
			t.Fatalf("hours %v gaps %v", hours, gaps)
		}
	}
}

func TestBuildPluginBlueprintRejectsMalformedInput(t *testing.T) {
	base := func() plugins.BlueprintSpec {
		return plugins.BlueprintSpec{Title: "T", Description: "d", ScheduleTemplate: "0 9 * * *", PromptTemplate: "hi", Category: "general", DeliverDefault: "origin"}
	}
	cases := []struct {
		edit   func(*plugins.BlueprintSpec)
		reason string
	}{
		{func(s *plugins.BlueprintSpec) { s.PromptTemplate = "hello {nobody}" }, "nobody"},
		{func(s *plugins.BlueprintSpec) {
			s.Slots = []any{map[string]any{"name": "x", "type": "colour", "label": "X"}}
		}, "unknown slot type 'colour' (slot x)"},
		{func(s *plugins.BlueprintSpec) { s.ScheduleTemplate = "not a schedule" }, "schedule"},
		{func(s *plugins.BlueprintSpec) { s.ScheduleTemplate = "{minute} {hour} * * *" }, "needs a slot named 'time'"},
		{func(s *plugins.BlueprintSpec) { s.Title = "" }, "title must be a non-empty string"},
		{func(s *plugins.BlueprintSpec) { s.Slots = []any{map[string]any{"name": "x", "type": "text"}} }, "slot 'x': BlueprintSlot.__init__() missing 1 required positional argument: 'label'"},
		{func(s *plugins.BlueprintSpec) { s.Slots = []any{map[string]any{"name": "x", "colour": 1}} }, "unknown slot field(s): colour"},
		{func(s *plugins.BlueprintSpec) { s.Slots = []any{"x"} }, "slot must be a mapping, got str"},
		{func(s *plugins.BlueprintSpec) {
			s.Slots = []any{map[string]any{"name": "1x", "type": "text", "label": "X"}}
		}, "invalid slot name '1x'"},
		{func(s *plugins.BlueprintSpec) { s.PromptTemplate = "hi {}" }, "prompt_template placeholder {} must be a plain slot name"},
		{func(s *plugins.BlueprintSpec) {
			s.PromptTemplate = "hi {x}"
			s.Slots = []any{map[string]any{"name": "x", "type": "text", "label": "X", "optional": true}}
		}, "prompt_template uses optional slot 'x' that has no default"},
		{func(s *plugins.BlueprintSpec) { s.Skills = []string{" "} }, "skills must be a list of non-empty strings"},
	}
	for _, c := range cases {
		spec := base()
		c.edit(&spec)
		if _, _, err := BuildPluginBlueprint("teamtools", "bad", spec); err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Fatalf("reason %q got %v", c.reason, err)
		}
	}
	if _, _, err := BuildPluginBlueprint("teamtools", "other:spoof", base()); err == nil || !strings.Contains(err.Error(), "invalid key 'other:spoof'") {
		t.Fatalf("spoof %v", err)
	}
	key, bp, err := BuildPluginBlueprint("teamtools", "morning-brief", base())
	if err != nil || key != "teamtools:morning-brief" || bp.(AutomationBlueprint).Plugin != "teamtools" {
		t.Fatalf("built %q %v %v", key, bp, err)
	}
}
