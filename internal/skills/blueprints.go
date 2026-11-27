package skills

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
)

// Automation a skill's metadata.icebath.blueprint sets up: a job that runs the skill on a schedule
type Blueprint struct {
	Skill    string
	Schedule string
	// Where its output goes, origin for where it was set up
	Deliver string
	// Prompt of each run, nil for the skill's own
	Prompt *string
	// Runs its script without the agent
	NoAgent         bool
	Model, Provider *string
	// Toolsets the runs are offered, nil for the default ones
	Toolsets []string
}

// Blueprint of a SKILL.md text, nil when it has none; an error when its block is malformed
func parseBlueprint(content string) (*Blueprint, error) {
	stripped := strings.TrimLeftFunc(strings.TrimLeft(content, "\uFEFF"), unicode.IsSpace)
	if !strings.HasPrefix(stripped, "---") {
		return nil, nil
	}
	end := strings.Index(stripped[3:], "\n---")
	if end < 0 {
		return nil, nil
	}
	parsed, err := decodeYAML(stripped[3 : end+3])
	fm, _ := parsed.(map[string]any)
	if err != nil || len(fm) == 0 {
		return nil, nil
	}
	raw, ok := Frontmatter(fm).Meta()["blueprint"]
	if !ok || raw == nil {
		return nil, nil
	}
	spec := mapping(raw)
	if spec == nil {
		return nil, errors.New("metadata.icebath.blueprint in its SKILL.md must be a set of named values.")
	}
	schedule := strings.TrimSpace(text(spec["schedule"]))
	if schedule == "" {
		return nil, errors.New("it has no schedule.")
	}
	toolsets, isList := spec["enabled_toolsets"].([]any)
	if spec["enabled_toolsets"] != nil && !isList {
		return nil, errors.New("its enabled_toolsets must be a list.")
	}
	out := &Blueprint{Skill: strings.TrimSpace(text(fm["name"])), Schedule: schedule, Deliver: "origin", NoAgent: config.Truthy(spec["no_agent"])}
	if v, ok := spec["deliver"]; ok {
		out.Deliver = cmp.Or(strings.TrimSpace(text(v)), "origin")
	}
	if v, ok := spec["prompt"]; ok && v != nil {
		prompt := text(v)
		out.Prompt = &prompt
	}
	for key, target := range map[string]**string{"model": &out.Model, "provider": &out.Provider} {
		if config.Truthy(spec[key]) {
			value := strings.TrimSpace(text(spec[key]))
			*target = &value
		}
	}
	for _, t := range toolsets {
		if t != nil {
			out.Toolsets = append(out.Toolsets, text(t))
		}
	}
	return out, nil
}

// Blueprint of the first skill folder of the name anywhere in the profile's folder, nil when it is none
func (s *Scope) Blueprint(name string) (*Blueprint, error) {
	var found *Blueprint
	var problem error
	root := filepath.Clean(s.Dir())
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Name() != "SKILL.md" || filepath.Dir(p) == root || filepath.Base(filepath.Dir(p)) != name {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if !utf8.Valid(data) {
			problem = fmt.Errorf("%s is not text.", p)
			return fs.SkipAll
		}
		if found, problem = parseBlueprint(string(data)); problem != nil || found != nil {
			if found != nil && found.Skill == "" {
				found.Skill = name
			}
			return fs.SkipAll
		}
		return nil
	})
	return found, problem
}
