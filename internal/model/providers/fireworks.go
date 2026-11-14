package providers

import "context"

type fireworksProfile struct {
	base
}

func newFireworks(d Deps) Profile {
	return &fireworksProfile{base{spec: Spec{
		Name: "fireworks", Aliases: []string{"fireworks-ai", "fw"}, DisplayName: "Fireworks AI",
		Description: "Fireworks AI — OpenAI-compatible direct model API",
		SignupURL:   "https://app.fireworks.ai/settings/users/api-keys", EnvVars: []string{"FIREWORKS_API_KEY"},
		BaseURL: "https://api.fireworks.ai/inference/v1", AuthType: authTypeAPIKey,
		DefaultHeaders: map[string]string{"HTTP-Referer": "https://icebath.app", "X-Title": "icebath",
			"User-Agent": userAgent(d, "icebath")},
		DefaultAuxModel: "accounts/fireworks/models/glm-5p2",
		FallbackModels: []string{"accounts/fireworks/models/kimi-k2p6", "accounts/fireworks/models/glm-5p2",
			"accounts/fireworks/models/kimi-k2p7-code"},
	}}}
}

func (p *fireworksProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	r := f.Reasoning
	if r == nil {
		return Extras{}, nil
	}
	if !r.Enabled {
		return effortTop("none"), nil
	}
	if r.Effort != "" {
		return effortTop(r.Effort), nil
	}
	return Extras{}, nil
}
