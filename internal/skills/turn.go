package skills

import (
	"cmp"
	"context"
	"slices"

	"github.com/nickheyer/icebath/internal/prompt"
)

// Scope of a turn whose system prompt the env is made for: its profile, folder, surface, session and the tools offered
func (r *Registry) Turn(env *prompt.Env) *Scope {
	s := r.Scope(env.Config, env.Cwd)
	s.Platform, s.Session = cmp.Or(env.Platform, s.Platform), env.SessionID
	s.Tools, s.Toolsets = map[string]bool{}, map[string]bool{}
	toolsets := map[string]string{}
	if registry := r.shared().tools; registry != nil {
		for _, def := range registry.Tools() {
			toolsets[def.Name] = def.Toolset
		}
	}
	for _, name := range env.Tools {
		s.Tools[name] = true
		if toolset := toolsets[name]; toolset != "" {
			s.Toolsets[toolset] = true
		}
	}
	return s
}

// What the index of a turn is built for besides its scope
func options(env *prompt.Env) IndexOptions {
	opts := IndexOptions{OneShot: env.OneShot}
	if env.Focus {
		opts.Demoted = map[string]bool{}
		for _, category := range NonCoding {
			opts.Demoted[category] = true
		}
	}
	return opts
}

// Whether the turn offers a tool of skills, without which skills are not offered at all
func offering(env *prompt.Env) bool {
	return slices.ContainsFunc(Tools, env.Offers)
}

// The skills index of a turn's system prompt, the profile's built in skills synced first; none when no tool of skills is offered
func (r *Registry) Index(ctx context.Context, env *prompt.Env) (string, error) {
	if !offering(env) {
		return "", nil
	}
	s := r.Turn(env)
	if err := s.Seed(ctx); err != nil {
		return "", err
	}
	return s.Index(ctx, options(env)), nil
}

// Whether the turn's index lists the skill and skill_view can load it, as the agent's help guidance asks when the icebath-agent skill
// is there
func (r *Registry) Listed(ctx context.Context, env *prompt.Env, name string) (bool, error) {
	if !env.Offers("skill_view") {
		return false, nil
	}
	index, err := r.Index(ctx, env)
	if err != nil {
		return false, err
	}
	_, ok := listing(index)[name]
	return ok, nil
}

// A note for the turn about to run when the skills differ from those the session's system prompt lists by something the model was not
// told yet in the user messages of the history; none when there is nothing to tell
func (r *Registry) Changes(ctx context.Context, env *prompt.Env, system string, history []string) string {
	if !offering(env) {
		return ""
	}
	s := r.Turn(env)
	return s.Changes(ctx, system, options(env), history)
}
