package cards

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/plugin"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Skill a card asks the app to run in the session on the stage
type InvokeSkillMsg struct {
	Name string
	Args string
}

// Skills every source offers, with who may run each, run or opened in place
type Skills struct {
	*lister
	env  *Env
	list []*plugin.Skill
}

func NewSkills(env *Env) *Skills {
	s := &Skills{env: env}
	t := env.Theme
	l := &lister{env: env, title: "SKILLS", glyph: t.Glyphs.Plan, id: "skills", ctx: keys.Skills, primary: "skills.run", cols: []render.Column{{Title: "SKILL", Max: 32}, {Title: "WHO", Max: 8}, {Title: "FROM", Max: 16}, {Title: "DESCRIPTION", Flex: 1}}}
	l.rows = func() [][]string {
		s.list = nil
		if lib := env.Editor.Library(); lib != nil {
			s.list = lib.Skills
		}
		var rows [][]string
		for _, sk := range s.list {
			who := "both"
			switch {
			case sk.DisableModelInvocation && !sk.UserInvocable:
				who = "nobody"
			case sk.DisableModelInvocation:
				who = "person"
			case !sk.UserInvocable:
				who = "model"
			}
			from := sk.Origin
			if sk.Plugin != nil {
				from = sk.Plugin.ID
			}
			rows = append(rows, []string{sk.Name, who, from, render.Summary(sk.Description, 80, t)})
		}
		return rows
	}
	l.detail = func(i int) []string {
		if i < 0 || i >= len(s.list) {
			return nil
		}
		sk := s.list[i]
		out := []string{render.KV("file", sk.File, t), render.KV("description", sk.Description, t)}
		if sk.ArgumentHint != "" {
			out = append(out, render.KV("arguments", sk.ArgumentHint, t))
		}
		if len(sk.AllowedTools) > 0 {
			out = append(out, render.KV("allowed tools", strings.Join(sk.AllowedTools, ", "), t))
		}
		if sk.Forked() {
			agent := sk.Agent
			if agent == "" {
				agent = "general-purpose"
			}
			out = append(out, render.KV("runs in", "agent "+agent, t))
		}
		if len(sk.Paths) > 0 {
			out = append(out, render.KV("loads for", strings.Join(sk.Paths, ", "), t))
		}
		for _, p := range sk.Problems {
			out = append(out, render.Warm(p, t))
		}
		return out
	}
	l.empty = func() string { return "no skills found under the skill directories or in any enabled plugin" }
	l.actions = map[string]func(*lister, int) tea.Cmd{
		"skills.run": func(l *lister, i int) tea.Cmd {
			if i < 0 || i >= len(s.list) {
				return nil
			}
			sk := s.list[i]
			if !sk.UserInvocable {
				return wm.Notify("skill "+sk.Name+" is run only by the model", wm.Warn)
			}
			if sk.ArgumentHint == "" {
				return func() tea.Msg { return InvokeSkillMsg{Name: sk.Name} }
			}
			return wm.RequestOverlay(wm.NewPrompt(t, sk.ArgumentHint, "", func(text string) tea.Cmd {
				return func() tea.Msg { return InvokeSkillMsg{Name: sk.Name, Args: strings.TrimSpace(text)} }
			}))
		},
		"skills.open": func(l *lister, i int) tea.Cmd {
			if i < 0 || i >= len(s.list) {
				return nil
			}
			file := s.list[i].File
			return func() tea.Msg { return CommandMsg{Name: "terminal", Args: "less " + shellQuote(file)} }
		},
	}
	s.lister = l
	return s
}

func (s *Skills) Title() string { return "SKILLS" }
func (s *Skills) Key() string   { return "skills" }

func (s *Skills) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	_, cmd := s.lister.Update(msg)
	return s, cmd
}
