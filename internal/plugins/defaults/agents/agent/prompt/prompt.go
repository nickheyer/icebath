package prompt

import (
	"context"

	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/skills"
)

// The agent's sections of the system prompt: who it is and how it works, the project it works in, and the moment the prompt is built;
// with the agent, handed in, as what keeps the prompts of sessions
type sections struct {
	prompt.Keeper
	probe *probe
	// Skills of the profiles, whose icebath-agent skill the help guidance points at when the turn lists it
	skills *skills.Registry
}

func Register(registry *prompt.Registry, keeper prompt.Keeper, skillset *skills.Registry) {
	registry.Register(sections{Keeper: keeper, probe: new(probe), skills: skillset})
}

func (s sections) Sections(ctx context.Context, env *prompt.Env) ([]prompt.Section, error) {
	help := helpGuidance
	if listed, err := s.skills.Listed(ctx, env, "icebath-agent"); err != nil {
		return nil, err
	} else if listed {
		help = helpSkillGuidance
	}
	out := []prompt.Section{
		{Tier: prompt.Stable, Name: "identity", Text: identity(ctx, env)},
		{Tier: prompt.Stable, Name: "help", Text: help},
	}
	out = append(out, guidance(env)...)
	coding := posture(ctx, env)
	out = append(out, prompt.Section{Tier: prompt.Stable, Name: "coding guidance", Text: coding.guidance})
	out = append(out, prompt.Section{Tier: prompt.Project, Name: "context files", Text: contextFiles(ctx, env)})
	// What follows the workspace snapshot goes with it in the project tier, or after the guidance when there is none
	tier := prompt.Stable
	if coding.workspace != "" {
		tier = prompt.Project
	}
	out = append(out,
		prompt.Section{Tier: prompt.Project, Name: "workspace", Text: coding.workspace},
		prompt.Section{Tier: tier, Name: "coding instructions", Text: coding.instructions},
		prompt.Section{Tier: tier, Name: "python toolchain", Text: s.probe.line(ctx, env)},
		prompt.Section{Tier: tier, Name: "platform hint", Text: platformHint(env)},
		prompt.Section{Tier: prompt.Turn, Name: "active profile", Text: activeProfile(env)},
		prompt.Section{Tier: prompt.Turn, Name: "timestamp", Text: timestamp(env)},
	)
	return out, nil
}
