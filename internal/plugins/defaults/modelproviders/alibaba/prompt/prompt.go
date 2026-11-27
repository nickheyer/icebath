package prompt

import (
	"context"
	"strings"

	alibabaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibaba/config"
	"github.com/nickheyer/icebath/internal/prompt"
)

type sections struct{}

func Register(registry *prompt.Registry) {
	registry.Register(sections{})
}

// The model's real name on Qwen Cloud, which answers to the API with another model's
func (sections) Sections(_ context.Context, env *prompt.Env) ([]prompt.Section, error) {
	if env.Kind != alibabaconfig.IntlName || env.Model == "" {
		return nil, nil
	}
	short := env.Model[strings.LastIndex(env.Model, "/")+1:]
	return []prompt.Section{{Tier: prompt.Stable, Name: "model identity", Text: "You are powered by the model named " + short + ". " +
		"The exact model ID is " + env.Model + ". " +
		"When asked what model you are, always answer based on this information, not on any model name returned by the API."}}, nil
}
