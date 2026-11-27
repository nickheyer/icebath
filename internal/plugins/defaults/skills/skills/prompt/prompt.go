package prompt

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/skills"
)

// Asks the agent to keep the workflows it works out as skills
const guidance = "When you work out a non-trivial workflow, record it with skill_manage for future reuse."

// The skills parts of the system prompt: the guidance to keep skills, the skills loaded into every session and those the session
// started with, and the index of the skills the agent can load
type sections struct {
	skills *skills.Registry
}

func Register(registry *prompt.Registry, skillset *skills.Registry) {
	registry.Register(sections{skills: skillset})
}

func (s sections) Sections(ctx context.Context, env *prompt.Env) ([]prompt.Section, error) {
	var out []prompt.Section
	if env.Offers("skill_manage") {
		out = append(out, prompt.Section{Tier: prompt.Stable, Name: "skills guidance", Text: guidance})
	}
	offered := slices.ContainsFunc(skills.Tools, env.Offers)
	if offered || len(env.Skills) > 0 {
		if err := s.skills.Turn(env).Seed(ctx); err != nil {
			return nil, err
		}
	}
	var loaded []string
	if offered && !env.IgnoreRules {
		text := env.Kept("skills auto load", func() string {
			text, names, missing := s.skills.Turn(env).AutoLoaded(ctx)
			loaded = names
			if len(missing) > 0 {
				env.Notify("These skills of Always loaded in /settings under Skills are missing or off, so they were left out: " + strings.Join(missing, ", ") + ".")
			}
			return text
		})
		out = append(out, prompt.Section{Tier: prompt.Stable, Name: "auto-loaded skills", Text: text})
	}
	if len(env.Skills) > 0 && !env.IgnoreRules {
		text := env.Kept("skills preload", func() string {
			already := map[string]bool{}
			for _, name := range loaded {
				already[name] = true
			}
			text, names, missing := s.skills.Turn(env).Preload(ctx, env.Skills, already)
			switch {
			case len(missing) > 0 && len(names) == 0:
				env.Notify("No skill was loaded, because " + skills.Missing(missing) + ".")
			case len(missing) > 0:
				env.Notify("Some skills were left out, because " + skills.Missing(missing) + ". Loaded: " + strings.Join(names, ", ") + ". Run icebath skills list to see your skills.")
			}
			return text
		})
		out = append(out, prompt.Section{Tier: prompt.Project, Name: "preloaded skills", Text: text})
	}
	index, err := s.skills.Index(ctx, env)
	if err != nil {
		return nil, err
	}
	return append(out, prompt.Section{Tier: prompt.Kept, Name: "skills", Text: index}), nil
}
