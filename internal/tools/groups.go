package tools

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// How a group of tools stands for a profile, the one answer every page, command and message gives
type Standing struct {
	Group Group
	// Its tools by the names a person reads
	Tools []Def
	// How many of its tools there are and how many are on
	Total, On int
	// Tools that are on and can't run, each with what it lacks
	Blocked []Blocked
	// On, Off, or how many of how many are on, such as 3 of 4 on
	State string
	// How many that are on can't run, such as 1 can't run; empty for none
	Note string
}

// A tool that is on and can't run, and what it lacks in a person's words
type Blocked struct {
	Tool Def
	Why  string
}

// Tools the profile's sessions may get, by their group: every profile's, and those of the profile itself
func (t *Tools) Of(cfg *config.Config) []Def {
	profile := cfg.Profile()
	return slices.DeleteFunc(t.Tools(), func(def Def) bool { return def.Profile != "" && def.Profile != profile })
}

// What a tool lacks to run for the profile, empty when it can run
func Lacking(def Def, env *Env) string {
	switch {
	case def.Schema == nil || def.Handler == nil:
		return "It is not set up in this build of icebath."
	case def.Check == nil || def.Check(env):
		return ""
	case def.Lacks != nil:
		if why := strs.Sentence(def.Lacks(env)); why != "" {
			return why
		}
	}
	return "Something it needs is missing."
}

// How every group of tools stands for the profile, in the order the groups declare, then by title: a group no plugin declares is one
// all the same, by the title its tools give
func (t *Tools) Standings(cfg *config.Config) []Standing {
	env := &Env{Config: cfg}
	disabled := cfg.GetStringSlice(toolsconfig.Disabled)
	defs := t.Of(cfg)
	titles := Groups(defs)
	var standings []Standing
	declared := t.Groups()
	find := func(title string) Group {
		for _, group := range declared {
			if group.Title == title && (group.Profile == "" || group.Profile == cfg.Profile()) {
				return group
			}
		}
		return Group{Title: title}
	}
	for _, def := range defs {
		title := titles[def.Toolset]
		at := slices.IndexFunc(standings, func(s Standing) bool { return s.Group.Title == title })
		if at < 0 {
			standings = append(standings, Standing{Group: find(title)})
			at = len(standings) - 1
		}
		s := &standings[at]
		s.Total++
		if slices.Contains(disabled, def.Name) {
			continue
		}
		s.On++
		if why := Lacking(def, env); why != "" {
			s.Blocked = append(s.Blocked, Blocked{Tool: def, Why: why})
		}
	}
	for i := range standings {
		s := &standings[i]
		for _, def := range defs {
			if titles[def.Toolset] == s.Group.Title {
				s.Tools = append(s.Tools, def)
			}
		}
		slices.SortFunc(s.Tools, func(a, b Def) int { return cmp.Compare(strings.ToLower(a.Label()), strings.ToLower(b.Label())) })
		switch {
		case s.On == s.Total:
			s.State = "On"
		case s.On == 0:
			s.State = "Off"
		default:
			s.State = fmt.Sprintf("%d of %d on", s.On, s.Total)
		}
		if n := len(s.Blocked); n > 0 {
			s.Note = fmt.Sprintf("%d can't run", n)
		}
	}
	slices.SortStableFunc(standings, func(a, b Standing) int {
		return cmp.Or(cmp.Compare(place(a.Group), place(b.Group)), cmp.Compare(strings.ToLower(a.Group.Title), strings.ToLower(b.Group.Title)))
	})
	return standings
}

// Place of a group among the groups: its own, those that give none after every one that does
func place(group Group) int {
	if group.Order == 0 {
		return 1 << 20
	}
	return group.Order
}

// What turning tools on or off came to: the tools turned, the words that named none, the groups named whose tools are turned on and off on
// the page of their area, and what to tell the person
type Turned struct {
	Tools     []Def
	Unknown   []string
	Elsewhere []string
	Said      string
}

// Turns on or off for the profile the tools the words name, the one way a tool or a group of them is turned on or off: a word names a tool
// by its name or its title, or every tool of a group by the group's title or a toolset's name. The tools of a group turned on and off on the
// page of its area are left to it
func (t *Tools) Turn(cfg *config.Config, words []string, on bool) (Turned, error) {
	defs := t.Of(cfg)
	titles := Groups(defs)
	elsewhere := map[string]bool{}
	for _, s := range t.Standings(cfg) {
		elsewhere[s.Group.Title] = s.Group.Elsewhere
	}
	var out Turned
	var names []string
	for _, word := range words {
		found := false
		for _, def := range defs {
			group := titles[def.Toolset]
			if !strings.EqualFold(word, def.Name) && !strings.EqualFold(word, def.Label()) && !strings.EqualFold(word, def.Toolset) &&
				!strings.EqualFold(word, group) {
				continue
			}
			found = true
			if elsewhere[group] {
				if !slices.Contains(out.Elsewhere, group) {
					out.Elsewhere = append(out.Elsewhere, group)
				}
				continue
			}
			if slices.Contains(names, def.Name) {
				continue
			}
			names, out.Tools = append(names, def.Name), append(out.Tools, def)
		}
		if !found {
			out.Unknown = append(out.Unknown, word)
		}
	}
	if len(names) > 0 {
		disabled := slices.DeleteFunc(cfg.GetStringSlice(toolsconfig.Disabled), func(name string) bool { return slices.Contains(names, name) })
		if !on {
			disabled = append(disabled, names...)
		}
		if err := Keep(cfg, disabled); err != nil {
			return out, err
		}
	}
	out.Said = said(out, on)
	return out, nil
}

// What a turn tells the person: the tool or how many tools were turned, the groups left to the page of their area, then the words that named
// none
func said(out Turned, on bool) string {
	state := "off"
	if on {
		state = "on"
	}
	var parts []string
	switch len(out.Tools) {
	case 0:
	case 1:
		parts = append(parts, out.Tools[0].Label()+" is now "+state+".")
	default:
		titles := make([]string, len(out.Tools))
		for i, def := range out.Tools {
			titles[i] = def.Label()
		}
		parts = append(parts, "Turned "+state+" "+strs.Count(len(out.Tools), "tool")+": "+strings.Join(titles, ", ")+".")
	}
	switch len(out.Elsewhere) {
	case 0:
	case 1:
		parts = append(parts, "The tools of "+out.Elsewhere[0]+" are turned on and off with the server, on its page.")
	default:
		parts = append(parts, "The tools of "+strs.And(out.Elsewhere)+" are turned on and off with their servers, on their pages.")
	}
	if len(out.Unknown) > 0 {
		parts = append(parts, "No tool or group of tools is called "+strs.Or(out.Unknown)+".")
	}
	return strings.Join(parts, " ")
}

// Keeps the list of the tools that are off for the profile, each once and in order: what turning tools on or off and every write of the
// setting end in
func Keep(cfg *config.Config, disabled []string) error {
	list := slices.Compact(slices.Sorted(slices.Values(disabled)))
	return cfg.Set(toolsconfig.Disabled, anyList(list))
}

func anyList(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Takes every write a person asks for of the tools that are off, so it ends in the one function that keeps them
func guardDisabled(cfg *config.Config) {
	cfg.Guard(toolsconfig.Disabled, config.Guard{Write: func(cfg *config.Config, key string, value any) (bool, error) {
		if value == nil || key != toolsconfig.Disabled {
			return false, nil
		}
		var names []string
		for _, item := range stringsOf(value) {
			if item = strings.TrimSpace(item); item != "" {
				names = append(names, item)
			}
		}
		return true, Keep(cfg, names)
	}})
}

// Items of a list value as text, whichever way it was read
func stringsOf(value any) []string {
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
	}
	return nil
}
