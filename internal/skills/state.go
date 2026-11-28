package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// An app a skill can be turned off in alone: the name a session's surface has, and what a person calls it
type Surface struct {
	Name, Title string
}

// Apps a skill can be turned off in alone
var Surfaces = []Surface{{"tui", "Terminal app"}, {"cli", "Command line"}}

// The apps as the names a table of them may hold, each with what a person calls it
func SurfaceOptions() []config.Option {
	options := make([]config.Option, len(Surfaces))
	for i, surface := range Surfaces {
		options[i] = config.Option{Value: surface.Name, Title: surface.Title}
	}
	return options
}

// Whether the app is one a skill can be turned off in alone
func known(surface string) bool {
	return slices.ContainsFunc(Surfaces, func(s Surface) bool { return s.Name == surface })
}

// Words a state of a skill is said in, on every page and in every command
const (
	On       = "On"
	Off      = "Off"
	OffHere  = "Off in this app"
	AlwaysOn = "Always on"
)

// State of a skill, by the name it loads by and the name its file gives, for the app given: always on for one the agent's instructions
// point at, off when it is off in every app, off in this app when it is off in the app alone, else on. The one function that says how a
// skill stands
func (s *Scope) State(load, name, surface string) string {
	if Essential[load] {
		return AlwaysOn
	}
	everywhere := namesOf(s.Config.Get(skillsconfig.Disabled))
	if everywhere[load] || everywhere[name] {
		return Off
	}
	if surface != "" {
		table, _ := s.Config.Get(skillsconfig.PlatformDisabled).(map[string]any)
		if here := namesOf(table[surface]); here[load] || here[name] {
			return OffHere
		}
	}
	return On
}

// How the skills of a category stand, from how many of them are on in the app: On, Off, or how many of how many are on
func CategoryState(on, total int) string {
	switch {
	case on == total:
		return On
	case on == 0:
		return Off
	}
	return fmt.Sprintf("%d of %d on", on, total)
}

// Category as a person reads it: the folder's name in words, Other for none
func CategoryTitle(category string) string {
	if category == "" {
		return "Other"
	}
	parts := strings.Split(category, "/")
	for i, part := range parts {
		words := strings.FieldsFunc(part, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
		for j, word := range words {
			if spelled, ok := spellings[strings.ToLower(word)]; ok {
				words[j] = spelled
			}
		}
		parts[i] = strs.Capital(strings.Join(words, " "))
	}
	return strings.Join(parts, " › ")
}

// Words of folder names written as they are spelled in a title, whatever case the folder gives them
var spellings = map[string]string{
	"ai": "AI", "api": "API", "apis": "APIs", "cli": "CLI", "devops": "DevOps", "github": "GitHub", "gpu": "GPU", "ios": "iOS", "llm": "LLM",
	"llms": "LLMs", "macos": "macOS", "mcp": "MCP", "ml": "ML", "mlops": "MLOps", "pdf": "PDF", "sql": "SQL", "ui": "UI", "ux": "UX",
}

// What a category of skills holds, as the DESCRIPTION.md of its folder says in the first skills folder that has one; empty when none does
func (s *Scope) CategoryAbout(category string) string {
	if category == "" {
		return ""
	}
	for _, dir := range s.Dirs() {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(category), "DESCRIPTION.md"))
		if err != nil {
			continue
		}
		fm, _ := Parse(string(data))
		if about := strings.Join(strings.Fields(str(fm["description"])), " "); about != "" {
			return about
		}
	}
	return ""
}

// Refusal of a skill that can't be turned off
func alwaysOn(name string) error {
	return fmt.Errorf("%s is always on, because the agent's instructions point at it.", name)
}

// Keeps the skills turned off in every app, or in the app alone, each once and in order: what turning skills on or off and every write of
// the settings a person asks for end in. A skill that is always on, and an app icebath does not know, are refused
func (s *Scope) KeepOff(names []string, surface string) error {
	set := map[string]bool{}
	for _, name := range names {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		if Essential[name] {
			return alwaysOn(name)
		}
		set[name] = true
	}
	if surface != "" && !known(surface) {
		return unknownApp(surface)
	}
	list := sorted(set)
	var err error
	switch {
	case surface == "" && len(list) == 0:
		err = s.Config.Set(skillsconfig.Disabled, nil)
	case surface == "":
		err = s.Config.Set(skillsconfig.Disabled, list)
	case len(list) == 0:
		err = s.Config.Set(skillsconfig.PlatformDisabled+"."+surface, nil)
	default:
		err = s.Config.Set(skillsconfig.PlatformDisabled+"."+surface, list)
	}
	s.registry.Forget()
	return err
}

// Refusal of an app icebath does not know
func unknownApp(surface string) error {
	var names []string
	for _, known := range Surfaces {
		names = append(names, known.Name+" for "+strs.Lower(known.Title))
	}
	return fmt.Errorf("A skill can't be turned off in “%s” alone. The apps are %s.", surface, strs.And(names))
}

// Turns skills on or off for the agent, in every app or in the app alone; the names that changed. Turning off a skill that is always on is
// refused
func (s *Scope) SetDisabled(names []string, enabled bool, surface string) ([]string, error) {
	if surface != "" && !known(surface) {
		return nil, unknownApp(surface)
	}
	current := namesOf(s.Config.Get(skillsconfig.Disabled))
	if surface != "" {
		table, _ := s.Config.Get(skillsconfig.PlatformDisabled).(map[string]any)
		current = namesOf(table[surface])
	}
	var changed []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		switch {
		case name == "":
		case Essential[name] && !enabled:
			return nil, alwaysOn(name)
		case Essential[name], current[name] == !enabled:
		default:
			current[name] = !enabled
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	off := make([]string, 0, len(current))
	for name, isOff := range current {
		if isOff {
			off = append(off, name)
		}
	}
	return changed, s.KeepOff(off, surface)
}

// Names of a list value, whichever way it was read
func listed(value any) []string {
	switch list := value.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	case string:
		return []string{list}
	}
	return nil
}

// Takes every write a person asks for of the skills that are off, in every app or in one, and of the trusted projects, so the page, the
// commands and the settings service end in the same functions and refuse the same values
func (r *Registry) Guard(cfg *config.Config) {
	cfg.Guard(skillsconfig.Disabled, config.Guard{
		Check: func(_ *config.Config, _ string, value any) error {
			for _, name := range listed(value) {
				if Essential[strings.TrimSpace(name)] {
					return alwaysOn(name)
				}
			}
			return nil
		},
		Write: func(cfg *config.Config, _ string, value any) (bool, error) {
			return true, r.Scope(cfg, "").KeepOff(listed(value), "")
		},
	})
	cfg.Guard(skillsconfig.PlatformDisabled, config.Guard{Check: r.checkApps, Write: r.writeApps})
	cfg.Guard(skillsconfig.TrustedProjectDirs, config.Guard{Check: r.checkTrusted, Write: r.writeTrusted})
}

// App and the rest of a key under the table of skills off in one app, both empty for the whole table
func appAt(key string) (surface, rest string) {
	surface, rest, _ = strings.Cut(strings.TrimPrefix(strings.TrimPrefix(key, skillsconfig.PlatformDisabled), "."), ".")
	return surface, rest
}

// Why a write of a key under the table of skills off in one app would be refused, nil when it would be kept
func (r *Registry) checkApps(_ *config.Config, key string, value any) error {
	surface, rest := appAt(key)
	tables := map[string]any{surface: value}
	switch {
	case rest != "":
		return errors.New("The skills off in an app are changed as one list.")
	case surface == "":
		tables, _ = value.(map[string]any)
	}
	for app, names := range tables {
		if !known(app) {
			return unknownApp(app)
		}
		for _, name := range listed(names) {
			if Essential[strings.TrimSpace(name)] {
				return alwaysOn(name)
			}
		}
	}
	return nil
}

// Makes a write a person asked for of a key under the table of skills off in one app through the one function that keeps them
func (r *Registry) writeApps(cfg *config.Config, key string, value any) (bool, error) {
	if err := r.checkApps(cfg, key, value); err != nil {
		return true, err
	}
	scope := r.Scope(cfg, "")
	if surface, _ := appAt(key); surface != "" {
		return true, scope.KeepOff(listed(value), surface)
	}
	tables, _ := value.(map[string]any)
	for _, known := range Surfaces {
		if err := scope.KeepOff(listed(tables[known.Name]), known.Name); err != nil {
			return true, err
		}
	}
	return true, nil
}

// Why a write of the trusted projects would be refused, nil when it would be kept: each new one must be a folder of this computer
func (r *Registry) checkTrusted(cfg *config.Config, _ string, value any) error {
	_, err := r.Scope(cfg, "").trustable(listed(value))
	return err
}

// Makes a write a person asked for of the trusted projects through the one function that keeps them
func (r *Registry) writeTrusted(cfg *config.Config, _ string, value any) (bool, error) {
	if value == nil {
		return true, r.Scope(cfg, "").KeepTrusted(nil)
	}
	return true, r.Scope(cfg, "").KeepTrusted(listed(value))
}
