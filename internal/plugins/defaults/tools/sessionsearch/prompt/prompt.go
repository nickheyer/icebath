package prompt

import (
	"context"

	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/sessionsearch/tools"
	"github.com/nickheyer/icebath/internal/prompt"
)

// Asks the agent to recall past conversations before asking the user to repeat them
const guidance = "When the user references something from a past conversation or you suspect relevant cross-session context exists, use " +
	"session_search to recall it before asking them to repeat themselves."

type sections struct{}

func Register(registry *prompt.Registry) {
	registry.Register(sections{})
}

func (sections) Sections(_ context.Context, env *prompt.Env) ([]prompt.Section, error) {
	if !env.Offers(tools.Name) {
		return nil, nil
	}
	return []prompt.Section{{Tier: prompt.Stable, Name: "session search guidance", Text: guidance}}, nil
}
