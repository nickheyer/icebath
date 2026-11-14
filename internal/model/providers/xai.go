package providers

import "github.com/nickheyer/icebath/internal/model/wire"

func newXAI(d Deps) Profile {
	return &base{spec: Spec{
		Name: "xai", Aliases: []string{"grok", "x-ai", "x.ai"}, Mode: wire.ModeResponses, EnvVars: []string{"XAI_API_KEY"},
		BaseURL: "https://api.x.ai/v1", AuthType: authTypeAPIKey,
		DefaultHeaders: map[string]string{"User-Agent": userAgent(d, "icebath")},
	}}
}
