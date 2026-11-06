package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Blueprint block present but malformed, the ValueError subclass of tools/blueprints.py
type BlueprintError struct{ Msg string }

func (e *BlueprintError) Error() string { return e.Msg }

// Parsed metadata.icebath.blueprint automation spec of a skill, nil pointers and slices standing for None
type BlueprintSpec struct {
	SkillName, Schedule, Deliver string
	Prompt                       *string
	NoAgent                      bool
	Model, Provider              *string
	EnabledToolsets              []string
	Raw                          map[string]any
}

// Parsed YAML frontmatter mapping of a SKILL.md, nil when absent or invalid
func blueprintSplitFrontmatter(text string) map[string]any {
	stripped := strings.TrimLeftFunc(strings.TrimLeft(text, "\uFEFF"), unicode.IsSpace)
	if !strings.HasPrefix(stripped, "---") {
		return nil
	}
	end := strings.Index(stripped[3:], "\n---")
	if end < 0 {
		return nil
	}
	data, err := yamlLoad(stripped[3 : end+3])
	if err != nil {
		return nil
	}
	m, _ := data.(map[string]any)
	return m
}

// Blueprint spec of a SKILL.md, nil when it is no blueprint and a BlueprintError when the block is malformed
func ParseBlueprint(skillMD string) (*BlueprintSpec, error) {
	fm := blueprintSplitFrontmatter(skillMD)
	if len(fm) == 0 {
		return nil, nil
	}
	raw, ok := icebathMeta(fm)["blueprint"]
	if !ok || raw == nil {
		return nil, nil
	}
	bp, ok := raw.(map[string]any)
	if !ok {
		return nil, &BlueprintError{"metadata.icebath.blueprint must be a mapping"}
	}
	get := func(k string, def any) any {
		if v, ok := bp[k]; ok {
			return v
		}
		return def
	}
	schedule := pystr.Strip(pyrepr.Str(get("schedule", "")))
	if schedule == "" {
		return nil, &BlueprintError{"blueprint.schedule is required and must be non-empty"}
	}
	toolsets := bp["enabled_toolsets"]
	list, isList := toolsets.([]any)
	if toolsets != nil && !isList {
		return nil, &BlueprintError{"blueprint.enabled_toolsets must be a list when present"}
	}
	spec := &BlueprintSpec{SkillName: pystr.Strip(pyrepr.Str(fmGet(fm, "name", ""))), Schedule: schedule,
		Deliver: pystr.Strip(pyrepr.Str(get("deliver", "origin"))), NoAgent: pyrepr.Truthy(get("no_agent", false)), Raw: bp}
	if spec.Deliver == "" {
		spec.Deliver = "origin"
	}
	if p, ok := bp["prompt"]; ok && p != nil {
		spec.Prompt = strPtr(pyrepr.Str(p))
	}
	if m := bp["model"]; pyrepr.Truthy(m) {
		spec.Model = strPtr(pystr.Strip(pyrepr.Str(m)))
	}
	if p := bp["provider"]; pyrepr.Truthy(p) {
		spec.Provider = strPtr(pystr.Strip(pyrepr.Str(p)))
	}
	for _, t := range list {
		spec.EnabledToolsets = append(spec.EnabledToolsets, pyrepr.Str(t))
	}
	return spec, nil
}

// Blueprint spec of the first <name>/SKILL.md anywhere under the skills tree, nil when none is a blueprint
func (s *Scope) BlueprintSpecForInstalled(skillName string) (*BlueprintSpec, error) {
	var found *BlueprintSpec
	var parseErr error
	filepath.WalkDir(s.SkillsDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != nil || parseErr != nil {
			return nil
		}
		if d.Name() != "SKILL.md" || filepath.Dir(path) == filepath.Clean(s.SkillsDir()) || filepath.Base(filepath.Dir(path)) != skillName {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if !utf8.Valid(data) {
			parseErr = fmt.Errorf("UnicodeDecodeError: 'utf-8' codec can't decode %s", path)
			return fs.SkipAll
		}
		spec, perr := ParseBlueprint(strings.TrimPrefix(string(data), "\uFEFF"))
		if perr != nil {
			parseErr = perr
			return fs.SkipAll
		}
		if spec != nil {
			if spec.SkillName == "" {
				spec.SkillName = skillName
			}
			found = spec
			return fs.SkipAll
		}
		return nil
	})
	return found, parseErr
}

func optStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Cron create_job kwargs of a spec, the one translation the job and suggestion paths share
func BlueprintToJobSpec(spec *BlueprintSpec, name string) *pyjson.Object {
	if name == "" {
		name = "blueprint:" + spec.SkillName
	}
	var skills, toolsets any
	if spec.SkillName != "" {
		skills = []any{spec.SkillName}
	}
	if spec.EnabledToolsets != nil {
		ts := make([]any, len(spec.EnabledToolsets))
		for i, t := range spec.EnabledToolsets {
			ts[i] = t
		}
		toolsets = ts
	}
	return objOf("prompt", optStr(spec.Prompt), "schedule", spec.Schedule, "name", name, "deliver", spec.Deliver,
		"skills", skills, "model", optStr(spec.Model), "provider", optStr(spec.Provider), "enabled_toolsets", toolsets,
		"no_agent", spec.NoAgent)
}

// Cron job creator with scheduler registration that pkg/cron installs at init
var cronCreateJobWithSchedulerRegistration = func(ctx context.Context, jobSpec *pyjson.Object) (map[string]any, error) {
	return nil, errors.New("cron blueprint hooks are not registered, import pkg/cron")
}

// Cron suggestion registrar that pkg/cron installs at init, nil record when skipped
var cronAddSuggestion = func(ctx context.Context, title, description, source string, jobSpec *pyjson.Object, dedupKey string) (map[string]any, error) {
	return nil, errors.New("cron blueprint hooks are not registered, import pkg/cron")
}

// Installs the cron job creator and suggestion registrar the blueprint bridge calls
func RegisterCronBlueprintHooks(create func(ctx context.Context, jobSpec *pyjson.Object) (map[string]any, error), add func(ctx context.Context, title, description, source string, jobSpec *pyjson.Object, dedupKey string) (map[string]any, error)) {
	cronCreateJobWithSchedulerRegistration, cronAddSuggestion = create, add
}

// Job creator of the blueprint bridge, the cron scheduler registration in production
var blueprintCreateJob = func(ctx context.Context, jobSpec *pyjson.Object) (map[string]any, error) {
	return cronCreateJobWithSchedulerRegistration(ctx, jobSpec)
}

// Suggestion registrar of the blueprint bridge, the cron suggestions store in production
var blueprintAddSuggestion = func(ctx context.Context, title, description, source string, jobSpec *pyjson.Object, dedupKey string) (map[string]any, error) {
	return cronAddSuggestion(ctx, title, description, source, jobSpec, dedupKey)
}

// Creates the cron job of a spec with the skill preloaded and returns the job
func CreateBlueprintJob(ctx context.Context, spec *BlueprintSpec, origin map[string]any, name string) (map[string]any, error) {
	jobSpec := BlueprintToJobSpec(spec, name)
	if origin != nil {
		jobSpec.Set("origin", origin)
	}
	return blueprintCreateJob(ctx, jobSpec)
}

// Registers an installed blueprint as a suggested cron job, nil when skipped
func RegisterBlueprintSuggestion(ctx context.Context, spec *BlueprintSpec) (map[string]any, error) {
	if spec.SkillName == "" {
		return nil, nil
	}
	deliver := ""
	if spec.Deliver != "" && spec.Deliver != "origin" {
		deliver = ", delivering to " + spec.Deliver
	}
	return blueprintAddSuggestion(ctx, fmt.Sprintf("Schedule '%s'", spec.SkillName),
		fmt.Sprintf("The '%s' blueprint runs on schedule %s%s.", spec.SkillName, spec.Schedule, deliver),
		"blueprint", BlueprintToJobSpec(spec, ""), fmt.Sprintf("blueprint:%s:%s", spec.SkillName, spec.Schedule))
}

// SKILL.md text of a cron job with its blueprint block, the inverse of CreateBlueprintJob
func ExportBlueprint(job map[string]any, body, blueprintName string) (string, error) {
	var raw any = blueprintName
	if blueprintName == "" {
		raw = job["name"]
		if !pyrepr.Truthy(raw) {
			raw = "shared-blueprint"
		}
	}
	var b strings.Builder
	for _, r := range pystr.Lower(pyrepr.Str(raw)) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-_")
	if name == "" {
		name = "shared-blueprint"
	}
	schedule := job["schedule_display"]
	if !pyrepr.Truthy(schedule) {
		rendered, err := scheduleToString(job["schedule"])
		if err != nil {
			return "", err
		}
		schedule = rendered
	}
	block := map[string]any{"schedule": schedule}
	keys := []string{"schedule"}
	add := func(k string, v any) {
		block[k] = v
		keys = append(keys, k)
	}
	if d := job["deliver"]; pyrepr.Truthy(d) && d != "origin" {
		add("deliver", d)
	}
	if p := job["prompt"]; pyrepr.Truthy(p) {
		add("prompt", p)
	}
	if pyrepr.Truthy(job["no_agent"]) {
		add("no_agent", true)
	}
	for _, k := range []string{"model", "provider", "enabled_toolsets"} {
		if v := job[k]; pyrepr.Truthy(v) {
			add(k, v)
		}
	}
	config.SetMappingKeys(block, keys)
	body = pystr.Strip(body)
	description := "Shared automation blueprint."
	if body != "" {
		first := []rune(pystr.Splitlines(body, false)[0])
		description = string(first[:min(200, len(first))])
	}
	icebath := map[string]any{"tags": []any{"blueprint", "automation"}, "blueprint": block}
	config.SetMappingKeys(icebath, []string{"tags", "blueprint"})
	frontmatter := map[string]any{"name": name, "description": description, "version": "1.0.0", "license": "MIT",
		"metadata": map[string]any{"icebath": icebath}}
	config.SetMappingKeys(frontmatter, []string{"name", "description", "version", "license", "metadata"})
	dumped, err := config.SafeDump(frontmatter)
	if err != nil {
		return "", err
	}
	bodyText := body
	if bodyText == "" {
		bodyText = fmt.Sprintf("# %s\n\nShared automation blueprint.", name)
	}
	return fmt.Sprintf("---\n%s\n---\n\n%s\n", pystr.Strip(string(dumped)), bodyText), nil
}

// Schedule string of a parsed schedule, a daily nine o'clock cron when nothing renders
func scheduleToString(schedule any) (string, error) {
	switch s := schedule.(type) {
	case string:
		return s, nil
	case map[string]any:
		kind := s["kind"]
		if kind == "cron" && pyrepr.Truthy(s["expr"]) {
			return pyrepr.Str(s["expr"]), nil
		}
		if kind == "interval" {
			if m := s["minutes"]; pyrepr.Truthy(m) {
				mins, err := pyIntOf(m)
				if err != nil {
					return "", err
				}
				if pyMod(mins, 60) == 0 {
					return fmt.Sprintf("every %dh", pyFloorDiv(mins, 60)), nil
				}
				return fmt.Sprintf("every %dm", mins), nil
			}
			if sec := s["seconds"]; pyrepr.Truthy(sec) {
				secs, err := pyIntOf(sec)
				if err != nil {
					return "", err
				}
				switch {
				case pyMod(secs, 3600) == 0:
					return fmt.Sprintf("every %dh", pyFloorDiv(secs, 3600)), nil
				case pyMod(secs, 60) == 0:
					return fmt.Sprintf("every %dm", pyFloorDiv(secs, 60)), nil
				}
				return fmt.Sprintf("every %ds", secs), nil
			}
		}
	}
	return "0 9 * * *", nil
}

// Python int() of a number or numeric string, a ValueError for anything else
func pyIntOf(v any) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		if n != n || n > 9.2e18 || n < -9.2e18 {
			return 0, fmt.Errorf("ValueError: cannot convert float %s to integer", pyrepr.Repr(n))
		}
		return int64(n), nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case string:
		t := strings.TrimSpace(n)
		if !strings.HasPrefix(t, "_") && !strings.HasSuffix(t, "_") && !strings.Contains(t, "__") {
			if out, err := strconv.ParseInt(strings.ReplaceAll(t, "_", ""), 10, 64); err == nil {
				return out, nil
			}
		}
		return 0, fmt.Errorf("ValueError: invalid literal for int() with base 10: %s", pyrepr.Repr(n))
	}
	return 0, fmt.Errorf("TypeError: int() argument must be a string, a bytes-like object or a real number, not '%T'", v)
}

func pyFloorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func pyMod(a, b int64) int64 { return a - pyFloorDiv(a, b)*b }
