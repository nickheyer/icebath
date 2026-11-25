package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
	schedule := strings.TrimSpace(anyutil.String(get("schedule", "")))
	if schedule == "" {
		return nil, &BlueprintError{"blueprint.schedule is required and must be non-empty"}
	}
	toolsets := bp["enabled_toolsets"]
	list, isList := toolsets.([]any)
	if toolsets != nil && !isList {
		return nil, &BlueprintError{"blueprint.enabled_toolsets must be a list when present"}
	}
	spec := &BlueprintSpec{SkillName: strings.TrimSpace(anyutil.String(fmGet(fm, "name", ""))), Schedule: schedule,
		Deliver: strings.TrimSpace(anyutil.String(get("deliver", "origin"))), NoAgent: anyutil.IsTrue(get("no_agent", false)), Raw: bp}
	if spec.Deliver == "" {
		spec.Deliver = "origin"
	}
	if p, ok := bp["prompt"]; ok && p != nil {
		spec.Prompt = strPtr(anyutil.String(p))
	}
	if m := bp["model"]; anyutil.IsTrue(m) {
		spec.Model = strPtr(strings.TrimSpace(anyutil.String(m)))
	}
	if p := bp["provider"]; anyutil.IsTrue(p) {
		spec.Provider = strPtr(strings.TrimSpace(anyutil.String(p)))
	}
	for _, t := range list {
		if t != nil {
			spec.EnabledToolsets = append(spec.EnabledToolsets, anyutil.String(t))
		}
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
			parseErr = fmt.Errorf("%s is not valid UTF-8", path)
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
func BlueprintToJobSpec(spec *BlueprintSpec, name string) *jsonutil.Object {
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

// Cron suggestion registrar that pkg/cron installs at init, nil record when skipped
var cronAddSuggestion = func(ctx context.Context, title, description, source string, jobSpec *jsonutil.Object, dedupKey string) (map[string]any, error) {
	return nil, errors.New("cron blueprint hooks are not registered, import pkg/cron")
}

// Installs the cron suggestion registrar the blueprint bridge calls
func RegisterCronBlueprintHooks(add func(ctx context.Context, title, description, source string, jobSpec *jsonutil.Object, dedupKey string) (map[string]any, error)) {
	cronAddSuggestion = add
}

// Suggestion registrar of the blueprint bridge, the cron suggestions store in production
var blueprintAddSuggestion = func(ctx context.Context, title, description, source string, jobSpec *jsonutil.Object, dedupKey string) (map[string]any, error) {
	return cronAddSuggestion(ctx, title, description, source, jobSpec, dedupKey)
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
